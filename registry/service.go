package registry

import (
	"fmt"
	"strconv"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/engine"
)

// Service exposes registry capabilities in the standard Engine
// interface. Once installed, it extends the engine with the
// following calls:
//
//  'auth': Authenticate against the public registry
//  'search': Search for images on the public registry
//  'pull': Download images from any registry (TODO)
//  'push': Upload images to any registry (TODO)
type Service struct {
	Config *ServiceConfig
}

// NewService returns a new instance of Service ready to be
// installed no an engine.
func NewService(options *Options) *Service {
	return &Service{
		Config: NewServiceConfig(options),
	}
}

// Install installs registry capabilities to eng.
func (s *Service) Install(eng *engine.Engine) error {
	eng.Register("auth", s.Auth)
	eng.Register("search", s.Search)
	eng.Register("resolve_repository", s.ResolveRepository)
	eng.Register("resolve_index", s.ResolveIndex)
	eng.Register("registry_config", s.GetRegistryConfig)
	return nil
}

// Auth contacts the public registry with the provided credentials,
// and returns OK if authentication was sucessful.
// It can be used to verify the validity of a client's credentials.
func (s *Service) Auth(job *engine.Job) engine.Status {
	var (
		authConfig = new(AuthConfig)
		endpoint   *Endpoint
		index      *IndexInfo
		status     string
		err        error
	)

	job.GetenvJson("authConfig", authConfig)

	addr := authConfig.ServerAddress
	if addr == "" {
		// Use the official registry address if not specified.
		addr = IndexServerAddress("")
	}
	if addr == "" {
		return job.Errorf("No configured registry to authenticate to.")
	}

	if index, err = ResolveIndexInfo(job, addr); err != nil {
		return job.Error(err)
	}

	if endpoint, err = NewEndpoint(index); err != nil {
		log.Errorf("unable to get new registry endpoint: %s", err)
		return job.Error(err)
	}

	authConfig.ServerAddress = endpoint.String()

	if status, err = Login(authConfig, endpoint, HTTPRequestFactory(nil)); err != nil {
		log.Errorf("unable to login against registry endpoint %s: %s", endpoint, err)
		return job.Error(err)
	}

	log.Infof("successful registry login for endpoint %s: %s", endpoint, status)
	job.Printf("%s\n", status)

	return engine.StatusOK
}


// Compare two items in the result table of search command.
// First compare the index name name. Second compare their rating. Then compare their name.
func cmpSearchResults(fst, snd *engine.Env) int {
	// First compare registries. If they are different, we know it's different repositories
	regA := fst.Get("registry_name")
	regB := snd.Get("registry_name")
	switch {
		case regA < regB:
			return -1
		case regA > regB:
			return 1
	}

	// If the indices in which we found the image differ, but the fully qualified names are identical, 
        // we don't compare stars, because they might be out of sync leading to dupes (e.g. in the case RH listing ISV content).
	// BTW: Does comparing stars make any sense whatshowever? How can that even be different for identical names?

	indA  := fst.Get("index_name")
	indB  := snd.Get("index_name")

	if indA == indB {
		starsA := fst.Get("star_count")
		starsB := snd.Get("star_count")

		intA, errA := strconv.ParseInt(starsA, 10, 64)
		intB, errB := strconv.ParseInt(starsB, 10, 64)
		if errA == nil && errB == nil {
			switch {
			case intA > intB:
				return -1
			case intA < intB:
				return 1
			}
		}
		switch {
			case starsA > starsB:
				return -1
			case starsA < starsB:
				return 1
		}
	}

	// Now comparing the names - this is what really matters.
	nameA := fst.Get("name")
	nameB := snd.Get("name")
	switch {
		case nameA < nameB:
			return -1
		case nameA > nameB:
			return 1
	}
	return 0
}

func searchTerm(job *engine.Job, outs *engine.Table, term string) error {
	var (
		metaHeaders = map[string][]string{}
		authConfig  = &AuthConfig{}
		registryName string
		resultName string
	)
	job.GetenvJson("authConfig", authConfig)
	job.GetenvJson("metaHeaders", metaHeaders)

	repoInfo, err := ResolveRepositoryInfo(job, term)
	if err != nil {
		return err
	}
	endpoint, err := repoInfo.GetEndpoint()
	if err != nil {
		return err
	}
	r, err := NewSession(authConfig, HTTPRequestFactory(metaHeaders), endpoint, true)
	if err != nil {
		return err
	}
	results, err := r.SearchRepositories(repoInfo.GetSearchTerm())
	if err != nil {
		return err
	}
	for _, result := range results.Results {
		out := &engine.Env{}
		// Check if search result has is fully qualified with registry
		// If not, assume REGISTRY = INDEX
		if RepositoryNameHasIndex(result.Name) {		
			registryName, resultName = splitReposName(result.Name, false)
			result.Name = resultName
		} else {
			registryName = repoInfo.Index.Name
		}
		out.Import(result)
		// Now add the index in which we found the result to the json. (not sure this is really the right place for this)
		out.Set("registry_name",registryName)
		out.Set("index_name",repoInfo.Index.Name)
		outs.Add(out)
	}
	return nil
}

// Search queries the public registry for images matching the specified
// search terms, and returns the results.
//
// Argument syntax: search TERM
//
// Option environment:
//	'authConfig': json-encoded credentials to authenticate against the registry.
//		The search extends to images only accessible via the credentials.
//
//	'metaHeaders': extra HTTP headers to include in the request to the registry.
//		The headers should be passed as a json-encoded dictionary.
//
// Output:
//	Results are sent as a collection of structured messages (using engine.Table).
//	Each result is sent as a separate message.
//	Results are ordered by:
//	    1. registry's index name
//          2. number of stars on registry
//          3. registry's name
func (s *Service) Search(job *engine.Job) engine.Status {
	if n := len(job.Args); n != 1 {
		return job.Errorf("Usage: %s TERM", job.Name)
	}
	var (
		term = job.Args[0]
		outs = engine.NewTableWithCmpFunc(cmpSearchResults, 0)
	)

	// helper for concurrent queries
	searchRoutine := func(term string, c chan<- error) {
		err := searchTerm(job, outs, term)
		c <- err
	}

	if RepositoryNameHasIndex(term) {
		if err := searchTerm(job, outs, term); err != nil {
			return job.Error(err)
		}
	} else if len(RegistryList) < 1 {
		return job.Errorf("No configured repository to search.")
	} else {
		var (
			err              error
			successfulSearch = false
			resultChan       = make(chan error)
		)
		// query all registries in parallel
		for i, r := range RegistryList {
			if i > 0 {
				job.Args[0] = fmt.Sprintf("%s/%s", r, term)
			} else {
				job.Args[0] = term
			}
			go searchRoutine(job.Args[0], resultChan)
		}
		for _ = range RegistryList {
			err = <-resultChan
			if err == nil {
				successfulSearch = true
			} else {
				log.Errorf("%s", err.Error())
			}
		}
		if !successfulSearch {
			return job.Error(err)
		}
	}
	outs.Sort()
	if _, err := outs.WriteListTo(job.Stdout); err != nil {
		return job.Error(err)
	}
	return engine.StatusOK
}

// ResolveRepository splits a repository name into its components
// and configuration of the associated registry.
func (s *Service) ResolveRepository(job *engine.Job) engine.Status {
	var (
		reposName = job.Args[0]
	)

	repoInfo, err := s.Config.NewRepositoryInfo(reposName)
	if err != nil {
		return job.Error(err)
	}

	out := engine.Env{}
	err = out.SetJson("repository", repoInfo)
	if err != nil {
		return job.Error(err)
	}
	out.WriteTo(job.Stdout)

	return engine.StatusOK
}

// Convenience wrapper for calling resolve_repository Job from a running job.
func ResolveRepositoryInfo(jobContext *engine.Job, reposName string) (*RepositoryInfo, error) {
	job := jobContext.Eng.Job("resolve_repository", reposName)
	env, err := job.Stdout.AddEnv()
	if err != nil {
		return nil, err
	}
	if err := job.Run(); err != nil {
		return nil, err
	}
	info := RepositoryInfo{}
	if err := env.GetJson("repository", &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// ResolveIndex takes indexName and returns index info
func (s *Service) ResolveIndex(job *engine.Job) engine.Status {
	var (
		indexName = job.Args[0]
	)

	index, err := s.Config.NewIndexInfo(indexName)
	if err != nil {
		return job.Error(err)
	}

	out := engine.Env{}
	err = out.SetJson("index", index)
	if err != nil {
		return job.Error(err)
	}
	out.WriteTo(job.Stdout)

	return engine.StatusOK
}

// Convenience wrapper for calling resolve_index Job from a running job.
func ResolveIndexInfo(jobContext *engine.Job, indexName string) (*IndexInfo, error) {
	job := jobContext.Eng.Job("resolve_index", indexName)
	env, err := job.Stdout.AddEnv()
	if err != nil {
		return nil, err
	}
	if err := job.Run(); err != nil {
		return nil, err
	}
	info := IndexInfo{}
	if err := env.GetJson("index", &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// GetRegistryConfig returns current registry configuration.
func (s *Service) GetRegistryConfig(job *engine.Job) engine.Status {
	out := engine.Env{}
	err := out.SetJson("config", s.Config)
	if err != nil {
		return job.Error(err)
	}
	out.WriteTo(job.Stdout)

	return engine.StatusOK
}
