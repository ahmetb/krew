package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/apex/gateway"
	"github.com/google/go-github/v32/github"
	"golang.org/x/oauth2"
)

const (
	orgName    = "kubernetes-sigs"
	repoName   = "krew-index"
	pluginsDir = "plugins"
)

type PluginCountResponse struct {
	Data struct {
		Count int `json:"count"`
	} `json:"data"`

	// TODO add error
}

func githubClient(ctx context.Context) *github.Client {
	var hc *http.Client
	if v := os.Getenv("GITHUB_ACCESS_TOKEN"); v != "" {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: v})
		hc = oauth2.NewClient(ctx, ts)
	}
	return github.NewClient(hc)
}

func pluginCountHandler(w http.ResponseWriter, req *http.Request) {
	_, dir, resp, err := githubClient(req.Context()).
		Repositories.GetContents(req.Context(), orgName, repoName, pluginsDir, &github.RepositoryContentGetOptions{})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, "error retrieving repo contents: %v", err) // JSON error
		return
	}
	yamls := filterYAMLs(dir)
	count := len(yamls)
	log.Printf("github response=%s count=%d rate: limit=%d remaining=%d",
		resp.Status, count, resp.Rate.Limit, resp.Rate.Remaining)

	var out PluginCountResponse
	out.Data.Count = count
	writeYAML(w, out)
}

func writeYAML(w io.Writer, v interface{}) {
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		log.Printf("json write error: %v", err)
	}
}

func filterYAMLs(entries []*github.RepositoryContent) []*github.RepositoryContent {
	var out []*github.RepositoryContent
	for _, v := range entries {
		if v == nil {
			continue
		}
		if v.GetType() == "file" && strings.HasSuffix(v.GetName(), ".yaml") {
			out = append(out, v)
		}
	}
	return out
}

func main() {
	port := flag.Int("port", -1, "specify a port to use http rather than AWS Lambda")
	flag.Parse()

	http.HandleFunc("/.netlify/functions/api/pc", pluginCountHandler)
	if *port == -1 {
		log.Fatal(gateway.ListenAndServe("n/a", nil))
	}
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}
