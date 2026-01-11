package config

import (
	"fmt"
	"os"
	"sort"
)

// ConfigExists checks if the config file exists
func ConfigExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ShouldAutoReload returns whether config auto-reload is enabled (default true)
func (c *Config) ShouldAutoReload() bool {
	if c.AutoReloadConfig == nil {
		return true
	}
	return *c.AutoReloadConfig
}

// LogRoutes prints the configured routes in a formatted table
func (c *Config) LogRoutes() {
	fmt.Printf("Routes (%d):\n", len(c.HTTP.K8s.Routes))

	type routeInfo struct {
		hostname string
		local    string
		target   string
		context  string
	}
	var routes []routeInfo
	maxLocal, maxTarget := 0, 0
	for hostname, route := range c.HTTP.K8s.Routes {
		scheme := route.Scheme
		if scheme == "" {
			scheme = "http"
		}
		local := fmt.Sprintf("%s://%s%s", scheme, hostname, c.HTTP.ListenAddr)
		var target string
		if route.Pod != "" {
			target = fmt.Sprintf("pod/%s:%d", route.Pod, route.Port)
		} else {
			target = fmt.Sprintf("%s:%d", route.Service, route.Port)
		}
		context := fmt.Sprintf("%s/%s", route.Context, route.Namespace)
		routes = append(routes, routeInfo{hostname, local, target, context})
		if len(local) > maxLocal {
			maxLocal = len(local)
		}
		if len(target) > maxTarget {
			maxTarget = len(target)
		}
	}
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].hostname < routes[j].hostname
	})
	for _, r := range routes {
		fmt.Printf("  %-*s  ->  %-*s  (%s)\n", maxLocal, r.local, maxTarget, r.target, r.context)
	}
}
