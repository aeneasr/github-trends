package trends

import (
	"bytes"
	"context"
	"fmt"
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

var defaultTTL = time.Hour * 24

func New(l *logrusx.Logger, token string) *Trends {
	var c http.Client
	if len(token) > 0 {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
		c = *oauth2.NewClient(context.Background(), ts)
	}

	cache, _ := ristretto.NewCache(&ristretto.Config{
		NumCounters: 1e7,
		MaxCost:     1 << 30,
		BufferItems: 64,
	})

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
	var gazers []*github.Stargazer
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

func (t *Trends) listGazers(ctx context.Context, user string) (gazers []*github.Stargazer, err error) {
	repos, err := t.getRepositories(ctx, user)
	if err != nil {
		return nil, err
	}

	for k := range repos {
		repo := repos[k]
		owner := *repo.Owner.Login
		name := *repo.Name

		t.l.WithField("owner", owner).WithField("repository", name).Debug("Checking stars for repository.")
		parts, err := t.getStargazers(ctx, owner, name)
		if err != nil {
			t.l.WithError(err).Error("Unable to fetch stargazers.")
			return nil, err
		}

		gazers = append(gazers, parts...)
		t.l.WithField("counts", len(gazers)).Info("Found stargazers.")
	}
	return
}

func repositoriesCacheKey(user string, page int) string {
	return fmt.Sprintf("repos/%s/%d", user, page)
}

type repositoriesCache struct {
	repos    []*github.Repository
	nextPage int
}

func (t *Trends) getRepositories(ctx context.Context, user string) (repos []*github.Repository, err error) {
	opt := &github.RepositoryListOptions{
		Sort:        "created",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		var parts []*github.Repository

		cacheKey := repositoriesCacheKey(user, opt.Page)
		if item, found := t.cache.Get(repositoriesCacheKey(user, opt.Page)); found {
			opt.Page = item.(*repositoriesCache).nextPage
			parts = item.(*repositoriesCache).repos
			t.l.WithField("cache_key", cacheKey).Debug("Found repositories in cache.")
		} else {
			p, resp, err := t.gc.Repositories.List(ctx, user, opt)
			if err != nil {
				return nil, err
			}
			t.cache.SetWithTTL(cacheKey, &repositoriesCache{repos: p, nextPage: resp.NextPage}, 0, defaultTTL)

			opt.Page = resp.NextPage
			parts = p
		}

		repos = append(repos, parts...)
		if opt.Page == 0 {
			break
		}
	}

	return
}

func stargazersCacheKey(user, repo string, page int) string {
	return fmt.Sprintf("gazers/%s/%s/%d", user, repo, page)
}

type stargazerCache struct {
	gazers   []*github.Stargazer
	nextPage int
}

func (t *Trends) getStargazers(ctx context.Context, user, repository string) (gazers []*github.Stargazer, err error) {
	opt := &github.ListOptions{PerPage: 100}
	for {
		var parts []*github.Stargazer

		cacheKey := stargazersCacheKey(user, repository, opt.Page)
		if item, found := t.cache.Get(cacheKey); found {
			opt.Page = item.(*stargazerCache).nextPage
			parts = item.(*stargazerCache).gazers
			t.l.WithField("cache_key", cacheKey).Debug("Found stargazers in cache.")
		} else {
			p, resp, err := t.gc.Activity.ListStargazers(context.Background(), user, repository, opt)
			if err != nil {
				return nil, err
			}
			opt.Page = resp.NextPage
			t.cache.SetWithTTL(cacheKey, &stargazerCache{gazers: p, nextPage: resp.NextPage}, 0, defaultTTL)
			parts = p
		}

		gazers = append(gazers, parts...)
		if opt.Page == 0 {
			break
		}
	}

	return
}

// IntValueFormatter is a ValueFormatter for int.
func IntValueFormatter(v interface{}) string {
	return fmt.Sprintf("%.0f", v)
}

func (t *Trends) renderSVG(gazers []*github.Stargazer) (*bytes.Buffer, error) {
	t.l.WithField("counts", len(gazers)).Info("Render SVG for repositories.")

	var series = chart.TimeSeries{
		Style: chart.Style{
			Show:        true,
			StrokeColor: drawing.Color{R: 129, G: 199, B: 239, A: 255},
			StrokeWidth: 2}}

	sorted := ByStars{Gazers: gazers}
	sort.Sort(sorted)

	for i, star := range sorted.Gazers {
		series.XValues = append(series.XValues, star.StarredAt.Time)
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

type ByStars struct{ Gazers []*github.Stargazer }

func (s ByStars) Less(i, j int) bool { return s.Gazers[i].StarredAt.Before(s.Gazers[j].StarredAt.Time) }

func (s ByStars) Len() int      { return len(s.Gazers) }
func (s ByStars) Swap(i, j int) { s.Gazers[i], s.Gazers[j] = s.Gazers[j], s.Gazers[i] }
