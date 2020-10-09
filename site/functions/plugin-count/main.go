package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/apex/gateway"
	"github.com/google/go-github/v32/github"
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

func pluginCountHandler(w http.ResponseWriter, req *http.Request) {
	gh := github.NewClient(nil)
	_, dir, resp, err := gh.Repositories.GetContents(req.Context(), orgName, repoName, pluginsDir,
		&github.RepositoryContentGetOptions{})
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

	if *port == -1 {
		log.Fatal(gateway.ListenAndServe("n/a", nil))
	}
	http.HandleFunc("/.netlify/functions/api", pluginCountHandler)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), nil))
}
