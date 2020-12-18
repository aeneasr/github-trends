package trends

import (
	"bytes"
	"context"
	"fmt"
	"golang.org/x/sync/errgroup"
	"net/http"
	"sort"
	"time"

	"github.com/dgraph-io/ristretto"
	"github.com/ory/x/logrusx"
	"golang.org/x/oauth2"

	"github.com/google/go-github/v33/github"
	"github.com/julienschmidt/httprouter"
	"github.com/wcharczuk/go-chart"
	"github.com/wcharczuk/go-chart/drawing"
)

type Trends struct {
	gc    *github.Client
	l     *logrusx.Logger
	cache *ristretto.Cache
}

var defaultTTL = time.Hour * 48

func New(l *logrusx.Logger, token string) *Trends {
	var c http.Client
	if len(token) > 0 {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
		c = *oauth2.NewClient(context.Background(), ts)
	}

	cache, _ := ristretto.NewCache(&ristretto.Config{
		NumCounters: 1e7, MaxCost: 150000000, BufferItems: 64})

	return &Trends{gc: github.NewClient(&c), l: l, cache: cache}
}

func (t *Trends) Register(r *httprouter.Router) {
	r.GET("/stars.svg", t.handleStars)
}

func sendSVG(w http.ResponseWriter, buff []byte) {
	w.Header().Add("cache-hit", "hit")
	w.Header().Add("content-type", "image/svg+xml;charset=utf-8")
	w.Header().Add("cache-control", "public, max-age=86400")
	_, _ = w.Write(buff)
}

func svgCacheKey(r *http.Request) string {
	return fmt.Sprintf("svg/%s/%s", r.URL.Query().Get("user"), r.URL.Query().Get("repo"))
}

func (t *Trends) handleStars(w http.ResponseWriter, r *http.Request, _ httprouter.Params) {
	user := r.URL.Query().Get("user")
	repo := r.URL.Query().Get("repo")

	if svg, found := t.cache.Get(svgCacheKey(r)); found {
		sendSVG(w, svg.([]byte))
		return
	}

	var err error
	var gazers []int64
	if len(repo) == 0 {
		gazers, err = t.listGazers(r.Context(), user)
	} else {
		gazers, err = t.getStargazers(r.Context(), user, repo)
	}

	if err != nil {
		t.l.WithError(err).Error("Unable to fetch data")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	svg, err := t.renderSVG(gazers)
	if err != nil {
		t.l.WithError(err).Error("Unable to render SVG")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// t.cache.SetWithTTL(svgCacheKey(r), svg.Bytes(), 0, defaultTTL)
	sendSVG(w, svg.Bytes())
}

func (t *Trends) listGazers(ctx context.Context, user string) (gazers []int64, err error) {
	repos, err := t.getRepositories(ctx, user)
	if err != nil {
		return nil, err
	}

	group, ctx := errgroup.WithContext(ctx)
	itemChan := make(chan []int64)

	for _, repo := range repos {
		func(repo repository) {
			group.Go(func() error {
				t.l.WithField("owner", repo.owner).WithField("repository", repo.name).Debug("Checking stars for repository.")
				parts, err := t.getStargazers(ctx, repo.owner, repo.name)
				if err != nil {
					t.l.WithError(err).Error("Unable to fetch stargazers.")
					return err
				}

				t.l.WithField("counts", len(parts)).WithField("repo", repo).Info("Found stargazers.")

				select {
				case itemChan <- parts:
				case <-ctx.Done():
					return ctx.Err()
				}

				return nil
			})
		}(repo)
	}

	go func() {
		_ = group.Wait()
		close(itemChan)
	}()

	for g := range itemChan {
		gazers = append(gazers, g...)
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return gazers, nil
}

func repositoriesCacheKey(user string, page int) string {
	return fmt.Sprintf("repos/%s/%d", user, page)
}

type repository struct {
	name  string
	owner string
}

type repositoriesCache struct {
	repos []repository
	*pagination
}

func (t *Trends) listRepositories(ctx context.Context, user string, page int) (*repositoriesCache, error) {
	cacheKey := repositoriesCacheKey(user, page)

	if item, found := t.cache.Get(cacheKey); found {
		t.l.WithField("cache_key", cacheKey).Debug("Found repository in cache.")
		return item.(*repositoriesCache), nil
	}

	p, resp, err := t.gc.Repositories.List(ctx, user, &github.RepositoryListOptions{Sort: "created",
		ListOptions: github.ListOptions{PerPage: 100, Page: page}})
	if err != nil {
		return nil, err
	}

	repos := make([]repository, len(p))
	for k, repo := range p {
		repos[k] = repository{name: *repo.Name, owner: *repo.Owner.Login}
	}

	item := &repositoriesCache{repos: repos, pagination: &pagination{
		LastPage: resp.LastPage,
		NextPage: resp.NextPage}}
	t.cache.SetWithTTL(cacheKey, item, 0, defaultTTL)

	return item, nil
}

func (t *Trends) getRepositories(ctx context.Context, user string) (repos []repository, err error) {
	item, err := t.listRepositories(ctx, user, 1)
	if err != nil {
		return nil, err
	}

	t.l.WithField("repos", item.repos).WithField("pagination", item.pagination).Debug("Found repos in pre-queue.")

	repos = append(repos, item.repos...)
	if item.NextPage == 0 {
		return repos, nil
	}

	group, ctx := errgroup.WithContext(ctx)
	itemChan := make(chan *repositoriesCache)

	for page := item.NextPage; page <= item.LastPage; page++ {
		func(page int) { // set page for subroutine
			group.Go(func() error {
				item, err := t.listRepositories(ctx, user, page)
				if err != nil {
					return err
				}
				t.l.WithField("repos", item.repos).WithField("page", page).WithField("pagination", item.pagination).Debug("Found repos in loop queue.")
				select {
				case itemChan <- item:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			})
		}(page)
	}

	go func() {
		_ = group.Wait()
		close(itemChan)
	}()

	for item := range itemChan {
		repos = append(repos, item.repos...)
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return repos, nil
}

func stargazersCacheKey(user, repo string, page int) string {
	return fmt.Sprintf("gazers/%s/%s/%d", user, repo, page)
}

type pagination struct {
	NextPage int
	LastPage int
}

type stargazerCache struct {
	gazers []int64
	*pagination
}

func (t *Trends) listStargazerPage(ctx context.Context, user, repository string, page int) (*stargazerCache, error) {
	cacheKey := stargazersCacheKey(user, repository, page)

	if item, found := t.cache.Get(cacheKey); found {
		t.l.WithField("cache_key", cacheKey).Debug("Found stargazers in cache.")
		return item.(*stargazerCache), nil
	}

	p, resp, err := t.gc.Activity.ListStargazers(ctx, user, repository, &github.ListOptions{PerPage: 100, Page: page})
	if err != nil {
		return nil, err
	}

	gazers := make([]int64, len(p))
	for k := range p {
		gazers[k] = p[k].StarredAt.Time.Unix()
	}

	item := &stargazerCache{gazers: gazers, pagination: &pagination{
		LastPage: resp.LastPage,
		NextPage: resp.NextPage,
	}}
	t.cache.SetWithTTL(cacheKey, item, 0, defaultTTL)

	return item, nil
}

func (t *Trends) getStargazers(ctx context.Context, user, repository string) (gazers []int64, err error) {
	item, err := t.listStargazerPage(ctx, user, repository, 1)
	if err != nil {
		return nil, err
	}

	t.l.WithField("gazers", item.gazers).WithField("repo", fmt.Sprintf("%s/%s", user, repository)).
		WithField("pagination", item.pagination).Debug("Found gazers in pre-queue.")

	gazers = append(gazers, item.gazers...)
	if item.NextPage == 0 {
		return gazers, nil
	}

	group, ctx := errgroup.WithContext(ctx)
	itemChan := make(chan *stargazerCache)

	for page := item.NextPage; page <= item.LastPage; page++ {
		func(page int) {
			group.Go(func() error {
				item, err := t.listStargazerPage(ctx, user, repository, page)
				if err != nil {
					return err
				}

				t.l.WithField("gazers", item.gazers).WithField("repo", fmt.Sprintf("%s/%s", user, repository)).
					WithField("pagination", item.pagination).Debug("Found gazers in page queue.")

				select {
				case itemChan <- item:
				case <-ctx.Done():
					return ctx.Err()
				}
				return nil
			})
		}(page)
	}

	go func() {
		_ = group.Wait()
		close(itemChan)
	}()

	for item := range itemChan {
		gazers = append(gazers, item.gazers...)
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	return gazers, nil
}

// IntValueFormatter is a ValueFormatter for int.
func IntValueFormatter(v interface{}) string {
	return fmt.Sprintf("%.0f", v)
}

func (t *Trends) renderSVG(gazers []int64) (*bytes.Buffer, error) {
	t.l.WithField("counts", len(gazers)).Info("Render SVG for repositories.")

	var series = chart.TimeSeries{
		Style: chart.Style{
			Show:        true,
			StrokeColor: drawing.Color{R: 129, G: 199, B: 239, A: 255},
			StrokeWidth: 2}}

	sort.Slice(gazers, func(i, j int) bool { return gazers[i] < gazers[j] })

	for i, star := range gazers {
		series.XValues = append(series.XValues, time.Unix(star, 0))
		series.YValues = append(series.YValues, float64(i))
	}

	if len(series.XValues) < 2 {
		t.l.WithField("counts", len(gazers)).Warn("Not enough results, adding some fake ones")
		series.XValues = append(series.XValues, time.Now())
		series.YValues = append(series.YValues, 1)
	}

	var graph = chart.Chart{
		XAxis: chart.XAxis{
			Name:      "Time",
			NameStyle: chart.StyleShow(),
			Style: chart.Style{
				Show:        true,
				StrokeWidth: 2,
				StrokeColor: drawing.Color{
					R: 85,
					G: 85,
					B: 85,
					A: 255,
				},
			},
		},
		YAxis: chart.YAxis{
			Name:      "Stargazers",
			NameStyle: chart.StyleShow(),
			Style: chart.Style{
				Show:        true,
				StrokeWidth: 2,
				StrokeColor: drawing.Color{
					R: 85,
					G: 85,
					B: 85,
					A: 255,
				},
			},
			ValueFormatter: IntValueFormatter,
		},
		Series: []chart.Series{series},
	}

	var b bytes.Buffer
	if err := graph.Render(chart.SVG, &b); err != nil {
		return nil, err
	}

	return &b, nil
}
