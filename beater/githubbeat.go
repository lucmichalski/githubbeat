package beater

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/publisher"

	"github.com/google/go-github/github"

	"github.com/jlevesy/githubbeat/config"
)

type Githubbeat struct {
	done     chan struct{}
	config   config.Config
	ghClient *github.Client
	client   publisher.Client
}

// Creates beater
func New(b *beat.Beat, cfg *common.Config) (beat.Beater, error) {
	config := config.DefaultConfig
	if err := cfg.Unpack(&config); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}

	return &Githubbeat{
		done:   make(chan struct{}),
		config: config,
	}, nil
}

func newGithubClient(accessToken string) (*github.Client, error) {
	if accessToken == "" {
		return github.NewClient(nil), nil
	}

	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: accessToken},
	)

	client := github.NewClient(oauth2.NewClient(ctx, ts))

	if _, _, err := client.Repositories.List(ctx, "", nil); err != nil {
		return nil, err
	}

	return client, nil
}

func (bt *Githubbeat) Run(b *beat.Beat) error {
	logp.Info("githubbeat is running! Hit CTRL-C to stop it.")

	bt.client = b.Publisher.Connect()

	ghClient, err := newGithubClient(bt.config.AccessToken)

	if err != nil {
		return err
	}

	bt.ghClient = ghClient

	ticker := time.NewTicker(bt.config.Period)

	rootCtx, cancelRootCtx := context.WithCancel(context.Background())

	for {
		select {
		case <-bt.done:
			cancelRootCtx()
			return nil
		case <-ticker.C:
			jobCtx, jobCancel := context.WithTimeout(rootCtx, bt.config.JobTimeout)
			defer jobCancel()
			if len(bt.config.Repos) > 0 {
				go bt.collectReposEvents(jobCtx, bt.config.Repos)
			}

			if len(bt.config.Orgs) > 0 {
				go bt.collectOrgsEvents(jobCtx, bt.config.Orgs)
			}
		}
	}
}

func (bt *Githubbeat) Stop() {
	bt.client.Close()
	close(bt.done)
}

func (bt *Githubbeat) collectOrgsEvents(ctx context.Context, orgs []string) {
	out := make(chan []*github.Repository, len(orgs))
	wg := sync.WaitGroup{}
	wg.Add(len(orgs))

	for _, org := range orgs {
		go func(ctx context.Context, org string, out chan<- []*github.Repository, wg *sync.WaitGroup) {
			res, _, err := bt.ghClient.Repositories.ListByOrg(ctx, org, nil)

			if err != nil {
				logp.Err("Failed to collect org repos listing, got :", err)
				wg.Done()
				return
			}

			out <- res
			wg.Done()
		}(ctx, org, out, &wg)
	}

	wg.Wait()
	close(out)

	for repos := range out {
		for _, repo := range repos {
			bt.client.PublishEvent(bt.newRepoEvent(repo))
		}
	}
}

func (bt *Githubbeat) collectReposEvents(ctx context.Context, repos []string) {
	out := make(chan common.MapStr, len(repos))
	wg := sync.WaitGroup{}

	wg.Add(len(repos))

	for _, repoName := range repos {
		go func(ctx context.Context, repo string, out chan<- common.MapStr, wg *sync.WaitGroup) {
			r := strings.Split(repo, "/")

			if len(r) != 2 {
				logp.Err("Invalid repo name format, expected [org]/[name], got: ", repo)
				wg.Done()
				return
			}

			res, _, err := bt.ghClient.Repositories.Get(ctx, r[0], r[1])

			if err != nil {
				logp.Err("Failed to collect event, got :", err)
				wg.Done()
				return
			}

			out <- bt.newRepoEvent(res)
			wg.Done()
		}(ctx, repoName, out, &wg)
	}

	wg.Wait()

	close(out)

	for event := range out {
		bt.client.PublishEvent(event)
	}
}

func (Githubbeat) newRepoEvent(repo *github.Repository) common.MapStr {
	return common.MapStr{
		"@timestamp":  common.Time(time.Now()),
		"type":        "githubbeat",
		"repo":        repo.GetName(),
		"owner":       repo.Owner.GetLogin(),
		"stargazers":  repo.GetStargazersCount(),
		"forks":       repo.GetForksCount(),
		"watchers":    repo.GetWatchersCount(),
		"issues":      repo.GetOpenIssuesCount(),
		"subscribers": repo.GetSubscribersCount(),
		"network":     repo.GetNetworkCount(),
		"size":        repo.GetSize(),
	}
}
