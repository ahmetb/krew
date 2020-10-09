// Copyright 2020 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/apex/gateway"
	"github.com/google/go-github/v32/github"
	"golang.org/x/oauth2"
	krew "sigs.k8s.io/krew/pkg/index"
	"sigs.k8s.io/yaml"
)

const (
	orgName    = "kubernetes-sigs"
	repoName   = "krew-index"
	pluginsDir = "plugins"

	urlFetchBatchSize = 40
	cacheSeconds      = 60 * 60
)

var (
	githubRepoPattern = regexp.MustCompile(`.*github\.com/([^/]+/[^/#]+)`)
)

type PluginCountResponse struct {
	Data struct {
		Count int `json:"count"`
	} `json:"data"`
	Error ErrorResponse `json:"error,omitempty"`
}

type pluginInfo struct {
	Name             string `json:"name,omitempty"`
	Homepage         string `json:"homepage,omitempty"`
	ShortDescription string `json:"short_description,omitempty"`
	GithubRepo       string `json:"github_repo,omitempty"`
}

type ErrorResponse struct {
	Message string `json:"message,omitempty"`
}

type PluginsResponse struct {
	Data struct {
		Plugins []pluginInfo `json:"plugins,omitempty"`
	} `json:"data,omitempty"`
	Error ErrorResponse `json:"error"`
}

func githubClient(ctx context.Context) *github.Client {
	var hc *http.Client

	// if not configured, you should configure a GITHUB_ACCESS_TOKEN
	// variable on Netlify dashboard for the site. You can create a
	// permission-less "personal access token" on GitHub account settings.
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
		writeJSON(w, PluginCountResponse{Error: ErrorResponse{Message: fmt.Sprintf("error retrieving repo contents: %v", err)}})
		return
	}
	yamls := filterYAMLs(dir)
	count := len(yamls)
	log.Printf("github response=%s count=%d rate: limit=%d remaining=%d",
		resp.Status, count, resp.Rate.Limit, resp.Rate.Remaining)

	var out PluginCountResponse
	out.Data.Count = count

	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheSeconds))
	writeJSON(w, out)
}

func writeJSON(w io.Writer, v interface{}) {
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

func loggingHandler(f http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		start := time.Now()
		log.Printf("[req]  > method=%s path=%s", req.Method, req.URL)
		defer func() {
			log.Printf("[resp] < method=%s path=%s took=%v", req.Method, req.URL, time.Since(start))
		}()
		f.ServeHTTP(w, req)
	})
}

func pluginsHandler(w http.ResponseWriter, req *http.Request) {
	_, dir, resp, err := githubClient(req.Context()).
		Repositories.GetContents(req.Context(), orgName, repoName, pluginsDir, &github.RepositoryContentGetOptions{})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, PluginsResponse{Error: ErrorResponse{Message: fmt.Sprintf("error retrieving repo contents: %v", err)}})
		return
	}
	log.Printf("github response=%s rate: limit=%d remaining=%d",
		resp.Status, resp.Rate.Limit, resp.Rate.Remaining)
	var out PluginsResponse

	plugins, err := fetchPlugins(filterYAMLs(dir))
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		writeJSON(w, PluginsResponse{Error: ErrorResponse{Message: fmt.Sprintf("failed to fetch plugins: %v", err)}})
		return
	}

	for _, v := range plugins {
		log.Printf("short description: %#v", v.Spec)
		pi := pluginInfo{
			Name:             v.Name,
			Homepage:         v.Spec.Homepage,
			ShortDescription: v.Spec.ShortDescription,
			GithubRepo:       findRepo(v.Spec.Homepage),
		}
		out.Data.Plugins = append(out.Data.Plugins, pi)
	}

	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", cacheSeconds))
	writeJSON(w, out)
}

func fetchPlugins(entries []*github.RepositoryContent) ([]*krew.Plugin, error) {
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu     sync.Mutex
		out    []*krew.Plugin
		retErr error
	)

	queue := make(chan string)
	var wg sync.WaitGroup

	for i := 0; i < urlFetchBatchSize; i++ {
		wg.Add(1)
		go func(j int) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case url, ok := <-queue:
					if !ok {
						return
					}
					p, err := readPlugin(url)
					if err != nil {
						retErr = err
						cancel()
						return
					}
					mu.Lock()
					out = append(out, p)
					mu.Unlock()
				}
			}
		}(i)
	}

	for _, v := range entries {
		url := v.GetDownloadURL()
		select {
		case <-ctx.Done():
			break
		case queue <- url:
		}
	}

	close(queue)
	wg.Wait()

	return out, retErr
}

func readPlugin(url string) (*krew.Plugin, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to get %s: %w", url, err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	var v krew.Plugin

	b, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read file %s: %w", url, err)
	}

	if err = yaml.Unmarshal(b, &v); err != nil {
		return nil, fmt.Errorf("failed to parse plugin manifest for %s: %w", url, err)
	}
	return &v, nil
}

func findRepo(homePage string) string {
	if matches := githubRepoPattern.FindStringSubmatch(homePage); matches != nil {
		return matches[1]
	}

	knownHomePages := map[string]string{
		`https://krew.sigs.k8s.io/`:                                  "kubernetes-sigs/krew",
		`https://sigs.k8s.io/krew`:                                   "kubernetes-sigs/krew",
		`https://kubernetes.github.io/ingress-nginx/kubectl-plugin/`: "kubernetes/ingress-nginx",
		`https://kudo.dev/`:                                          "kudobuilder/kudo",
		`https://kubevirt.io`:                                        "kubevirt/kubectl-virt-plugin",
		`https://popeyecli.io`:                                       "derailed/popeye",
		`https://soluble-ai.github.io/kubetap/`:                      "soluble-ai/kubetap",
	}
	return knownHomePages[homePage]
}

func main() {
	port := flag.Int("port", -1, "specify a port to use http rather than AWS Lambda")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/.netlify/functions/api/pluginCount", pluginCountHandler)
	mux.HandleFunc("/.netlify/functions/api/plugins", pluginsHandler)
	// To debug locally, you can run this server with -port=:8080 and run "hugo serve" and uncomment this:
	mux.Handle("/", httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: "localhost:1313"}))

	handler := loggingHandler(mux)
	if *port == -1 {
		log.Fatal(gateway.ListenAndServe("n/a", handler))
	}
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *port), handler))
}
