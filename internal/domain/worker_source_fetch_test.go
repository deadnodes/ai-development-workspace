package domain

import "testing"

func TestWorkerSourceFetchHardBounds(t *testing.T) {
	valid := WorkerSourceFetch{Provider: "github", RepositoryID: 42, TimeoutSeconds: 120, MaxDownloadBytes: 512 << 20, MaxRequests: 4096, MaxEntries: 20000, MaxDepth: 64}
	if !ValidWorkerSourceFetch(&valid, 256<<20) || ValidWorkerSourceFetch(nil, 1) {
		t.Fatal("valid source bounds rejected")
	}
	for name, change := range map[string]func(*WorkerSourceFetch){"provider": func(s *WorkerSourceFetch) { s.Provider = "other" }, "repository": func(s *WorkerSourceFetch) { s.RepositoryID = 0 }, "timeout": func(s *WorkerSourceFetch) { s.TimeoutSeconds = 121 }, "download": func(s *WorkerSourceFetch) { s.MaxDownloadBytes++ }, "requests": func(s *WorkerSourceFetch) { s.MaxRequests++ }, "entries": func(s *WorkerSourceFetch) { s.MaxEntries++ }, "depth": func(s *WorkerSourceFetch) { s.MaxDepth++ }} {
		t.Run(name, func(t *testing.T) {
			s := valid
			change(&s)
			if ValidWorkerSourceFetch(&s, 1) {
				t.Fatal("unsafe bound accepted")
			}
		})
	}
	if ValidWorkerSourceFetch(&valid, 0) || ValidWorkerSourceFetch(&valid, (256<<20)+1) {
		t.Fatal("invalid expanded source size accepted")
	}
}
