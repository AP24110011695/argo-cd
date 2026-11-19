package scm_provider

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strconv"

	"code.gitea.io/sdk/gitea"

	"github.com/argoproj/argo-cd/v3/util/proxy"
)

type GiteaProvider struct {
	client               *gitea.Client
	owner                string
	allBranches          bool
	excludeArchivedRepos bool
}

var _ SCMProviderService = &GiteaProvider{}

// giteaPageSize is an explicit page size for list calls: without it the server
// answers with its own DEFAULT_PAGING_NUM, silently dropping everything past
// the first page.
const giteaPageSize = 100

// giteaMaxPages caps the paging loops so a server that ignores pagination
// cannot make them loop forever (100 pages x 100 items covers any sane repo).
const giteaMaxPages = 100

// giteaPageDone reports whether enough items have been collected to stop
// paging: the server reports the collection size in the X-Total-Count header
// on every list response.
func giteaPageDone(resp *gitea.Response, collected int) bool {
	if resp == nil || resp.Response == nil {
		return true
	}
	if total, convErr := strconv.Atoi(resp.Response.Header.Get("X-Total-Count")); convErr == nil {
		return collected >= total
	}
	// no header: fall back to the page-full heuristic
	return collected > 0
}

func NewGiteaProvider(owner, token, url string, allBranches, insecure, excludeArchivedRepos bool, proxyURL, noProxy string) (*GiteaProvider, error) {
	if token == "" {
		token = os.Getenv("GITEA_TOKEN")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if insecure {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	transport.Proxy = proxy.GetCallback(proxyURL, noProxy)

	cookieJar, _ := cookiejar.New(nil)
	httpClient := &http.Client{Jar: cookieJar, Transport: transport}
	client, err := gitea.NewClient(url, gitea.SetToken(token), gitea.SetHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("error creating a new gitea client: %w", err)
	}
	return &GiteaProvider{
		client:               client,
		owner:                owner,
		allBranches:          allBranches,
		excludeArchivedRepos: excludeArchivedRepos,
	}, nil
}

func (g *GiteaProvider) GetBranches(_ context.Context, repo *Repository) ([]*Repository, error) {
	if !g.allBranches {
		branch, status, err := g.client.GetRepoBranch(g.owner, repo.Repository, repo.Branch)
		if status.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("got 404 while getting default branch %q for repo %q - check your repo config: %w", repo.Branch, repo.Repository, err)
		}
		if err != nil {
			return nil, err
		}
		return []*Repository{
			{
				Organization: repo.Organization,
				Repository:   repo.Repository,
				Branch:       repo.Branch,
				URL:          repo.URL,
				SHA:          branch.Commit.ID,
				Labels:       repo.Labels,
				RepositoryId: repo.RepositoryId,
			},
		}, nil
	}
	repos := []*Repository{}
	opts := gitea.ListRepoBranchesOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: giteaPageSize}}
	for range giteaMaxPages {
		branches, resp, err := g.client.ListRepoBranches(g.owner, repo.Repository, opts)
		if err != nil {
			return nil, err
		}
		for _, branch := range branches {
			repos = append(repos, &Repository{
				Organization: repo.Organization,
				Repository:   repo.Repository,
				Branch:       branch.Name,
				URL:          repo.URL,
				SHA:          branch.Commit.ID,
				Labels:       repo.Labels,
				RepositoryId: repo.RepositoryId,
			})
		}
		if len(branches) == 0 || giteaPageDone(resp, len(repos)) {
			break
		}
		opts.Page++
	}
	return repos, nil
}

func (g *GiteaProvider) ListRepos(_ context.Context, cloneProtocol string) ([]*Repository, error) {
	repos := []*Repository{}
	repoOpts := gitea.ListOrgReposOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: giteaPageSize}}
	for range giteaMaxPages {
		giteaRepos, repoResp, err := g.client.ListOrgRepos(g.owner, repoOpts)
		if err != nil {
			return nil, err
		}
		for _, repo := range giteaRepos {
			var url string
			switch cloneProtocol {
			// Default to SSH if unspecified (i.e. if "").
			case "", "ssh":
				url = repo.SSHURL
			case "https":
				url = repo.HTMLURL
			default:
				return nil, fmt.Errorf("unknown clone protocol for GitHub %v", cloneProtocol)
			}
			labels := []string{}
			labelOpts := gitea.ListLabelsOptions{ListOptions: gitea.ListOptions{Page: 1, PageSize: giteaPageSize}}
			for range giteaMaxPages {
				giteaLabels, labelResp, err := g.client.ListRepoLabels(g.owner, repo.Name, labelOpts)
				if err != nil {
					return nil, err
				}
				for _, label := range giteaLabels {
					labels = append(labels, label.Name)
				}
				if len(giteaLabels) == 0 || giteaPageDone(labelResp, len(labels)) {
					break
				}
				labelOpts.Page++
			}

			if g.excludeArchivedRepos && repo.Archived {
				continue
			}

			repos = append(repos, &Repository{
				Organization: g.owner,
				Repository:   repo.Name,
				Branch:       repo.DefaultBranch,
				URL:          url,
				Labels:       labels,
				RepositoryId: int(repo.ID),
			})
		}
		if len(giteaRepos) == 0 || giteaPageDone(repoResp, len(repos)) {
			break
		}
		repoOpts.Page++
	}
	return repos, nil
}

func (g *GiteaProvider) RepoHasPath(_ context.Context, repo *Repository, path string) (bool, error) {
	_, resp, err := g.client.GetContents(repo.Organization, repo.Repository, repo.Branch, path)
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		if err.Error() == "expect file, got directory" {
			return true, nil
		}
		return false, err
	}
	return true, nil
}
