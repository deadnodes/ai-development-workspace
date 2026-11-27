package application

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"releasecontrol/internal/auth"
	"releasecontrol/internal/buildconfig"
	"releasecontrol/internal/domain"
)

type autoHeadObservation struct{ profile, branch, sha string }
type autoHeadKey struct{}
type autoDeployKey struct{}

func autoBuildContext(ctx context.Context, p buildconfig.Profile, sha string) context.Context {
	return context.WithValue(autoContext(ctx, "system/auto-build", p.ProductID), autoHeadKey{}, autoHeadObservation{p.ID, p.DefaultBranch, sha})
}

type autoHead struct {
	seen     string
	pending  bool
	recovery bool
	warned   string
	polled   time.Time
	observed time.Time
}

// autoHeadSnapshot returns a detached copy; no caller retains map-owned pointers.
func (a *buildRuntime) autoHeadSnapshot(profile string) *autoHead {
	a.autoHeadsMu.RLock()
	defer a.autoHeadsMu.RUnlock()
	if h := a.autoHeads[profile]; h != nil {
		snapshot := *h
		return &snapshot
	}
	return nil
}

func (a *buildRuntime) setAutoHead(profile string, h *autoHead) {
	a.autoHeadsMu.Lock()
	defer a.autoHeadsMu.Unlock()
	if h == nil {
		delete(a.autoHeads, profile)
		return
	}
	if a.autoHeads == nil {
		a.autoHeads = map[string]*autoHead{}
	}
	snapshot := *h
	a.autoHeads[profile] = &snapshot
}

type commitTimeProvider interface {
	CommitTime(context.Context, buildconfig.Connection, string, string) (time.Time, error)
}

func enabled(v *bool) bool { return v != nil && *v }
func autoTrigger(actor string) string {
	if actor == "system/auto-build" || actor == "system/auto-deploy" {
		return "auto"
	}
	return ""
}
func autoContext(ctx context.Context, actor, product string) context.Context {
	return auth.WithPrincipal(ctx, auth.Principal{ID: actor, Permissions: []string{"write", "read"}, Products: []string{product}})
}
func autoDeliveryEnabled() bool {
	return !strings.EqualFold(strings.TrimSpace(os.Getenv("RCP_AUTO_DELIVERY")), "off")
}

// Called under the build runtime mutex. Provider calls and commands never hold a store lock.
func (s *Service) pollAutoBuilds(ctx context.Context, now time.Time) {
	a := s.builds
	if !autoDeliveryEnabled() {
		return
	}
	started := time.Now()
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, p := range a.config.Profiles {
		if !enabled(p.AutoBuild) && !enabled(p.AutoDeployDev) {
			continue
		}
		h := a.autoHeadSnapshot(p.ID)
		if h == nil {
			h = &autoHead{}
		}
		if !h.polled.IsZero() && now.Sub(h.polled) < 20*time.Second {
			continue
		}
		wg.Add(1)
		go func(p buildconfig.Profile, h *autoHead) {
			defer wg.Done()
			defer a.setAutoHead(p.ID, h)
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			// Account for time waiting behind the concurrency bound, so even late
			// profiles cannot be polled again within twenty seconds.
			h.polled = now.Add(time.Since(started))
			head, err := a.provider.BranchHead(ctx, a.config.Connection(p.ConnectionID), p.Repository, p.DefaultBranch)
			warn := func(key string, err error) {
				if h.warned != key {
					slog.Warn("automatic build observation failed", "profile", p.ID, "head", key, "error", err)
					h.warned = key
				}
			}
			if err != nil {
				key := h.seen
				if key == "" {
					key = "observation"
				}
				warn(key, err)
				return
			}
			if !buildconfig.SHA.MatchString(head) {
				warn(head, invalid("invalid provider HEAD"))
				return
			}
			h.observed = now.Add(time.Since(started))
			if h.seen == "" {
				// Establish the baseline even when recovery observation fails.
				h.seen, h.pending, h.recovery = head, true, true
			} else if h.seen != head {
				h.seen, h.pending, h.recovery, h.warned = head, true, false, ""
			}
			// Publish the fresh head before recovery or queue I/O can block.
			a.setAutoHead(p.ID, h)
			if !enabled(p.AutoBuild) || !h.pending {
				return
			}
			if h.recovery {
				exists := false
				err = s.view(ctx, func(st *domain.State) error {
					for _, b := range st.Builds {
						if b.ProductID == p.ProductID && b.ProfileID == p.ID && b.Branch == p.DefaultBranch && b.SourceSHA == head {
							exists = true
							break
						}
					}
					return nil
				})
				if err != nil {
					warn(head, err)
					return
				}
				if exists {
					h.pending = false
					return
				}
				observer, ok := a.provider.(commitTimeProvider)
				if !ok {
					warn(head, invalid("commit timestamp observation unavailable"))
					return
				}
				at, e := observer.CommitTime(ctx, a.config.Connection(p.ConnectionID), p.Repository, head)
				if e != nil {
					warn(head, e)
					return
				}
				if at.IsZero() || now.Sub(at) > 15*time.Minute || at.After(now) {
					h.pending = false
					return
				}
			}
			_, err = s.QueueBuild(autoBuildContext(ctx, p, head), domain.QueueBuildRequest{ProductID: p.ProductID, ProfileID: p.ID, Branch: p.DefaultBranch, RequestKey: "auto-build:" + p.ID + ":" + head})
			if err != nil {
				warn(head, err)
				return
			}
			h.pending = false
		}(p, h)
	}
	wg.Wait()
}

func compareBuildAge(build, other domain.Build) int {
	if order := build.QueuedAt.Compare(other.QueuedAt); order != 0 {
		return order
	}
	return build.CreatedAt.Compare(other.CreatedAt)
}
func (s *Service) autoDeployBuilds(ctx context.Context) {
	a := s.builds
	if !autoDeliveryEnabled() {
		return
	}
	if a.autoDone == nil {
		a.autoDone = map[string]bool{}
	}
	var st domain.State
	err := s.view(ctx, func(current *domain.State) error {
		for _, b := range current.Builds {
			st.Builds = append(st.Builds, copyBuild(b))
		}
		for _, d := range current.Deployments {
			st.Deployments = append(st.Deployments, copyDeployment(d))
		}
		return nil
	})
	if err != nil {
		slog.Warn("automatic deployment snapshot failed", "error", err)
		return
	}
	for _, p := range a.config.Profiles {
		if !enabled(p.AutoDeployDev) {
			continue
		}
		h := a.autoHeadSnapshot(p.ID)
		if h == nil || h.seen == "" || h.observed.IsZero() || time.Since(h.observed) > 2*time.Minute || time.Now().Before(h.observed) {
			continue
		}
		var newest domain.Build
		for _, b := range st.Builds {
			if b.ProductID == p.ProductID && b.ProfileID == p.ID && b.Branch == p.DefaultBranch && b.SourceSHA == h.seen && b.Status == "SUCCEEDED" && !b.RestoreQuarantined && (newest.ID == "" || compareBuildAge(b, newest) >= 0) {
				newest = b
			}
		}
		if newest.ID == "" || a.autoDone[newest.ID] {
			continue
		}
		// The security gate decides asynchronously after the build: wait for PASS, never
		// propose a deployment that the gate has refused.
		if p.RequireSecurityGate && newest.SecurityStatus != "PASS" {
			if newest.SecurityStatus == "FAIL" || newest.SecurityStatus == "ERROR" || newest.SecurityStatus == "MISSING" {
				a.autoDone[newest.ID] = true
				slog.Info("automatic deployment refused by security gate", "build", newest.ID, "security_status", newest.SecurityStatus)
			}
			continue
		}
		deployed := false
		for _, d := range st.Deployments {
			if d.ProductID == p.ProductID && d.EnvironmentID == "dev" && d.BuildID == newest.ID {
				deployed = true
				break
			}
		}
		latest := latestDevDeployment(&st, p.ProductID, p.ID)
		if deployed || latest.ID != "" && latest.SourceSHA == newest.SourceSHA {
			a.autoDone[newest.ID] = true
			continue
		}
		// DeployBuild performs the fresh ancestry check against the latest successful Dev
		// deployment and refuses rollback. Never supply allow_rollback.
		_, err = s.DeployBuild(context.WithValue(autoContext(ctx, "system/auto-deploy", p.ProductID), autoDeployKey{}, true), domain.DeployBuildRequest{ProductID: p.ProductID, BuildID: newest.ID, EnvironmentID: "dev", RequestKey: "auto-deploy:" + newest.ID})
		if err == nil {
			a.autoDone[newest.ID] = true
			continue
		}
		if strings.Contains(err.Error(), "ROLLBACK_REFUSED") {
			slog.Info("automatic deployment rollback refused", "build", newest.ID)
			a.autoDone[newest.ID] = true
		} else {
			slog.Warn("automatic deployment deferred", "build", newest.ID, "error", err)
		}
	}
}
