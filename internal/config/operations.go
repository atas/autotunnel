package config

import (
	"fmt"
	"os"
	"strings"
)

func FileExists(path string) bool {
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

func (c *Config) PrintRoutes() {
	fmt.Printf("Routes (%d):\n", len(c.HTTP.K8s.Routes))
	parts := strings.Split(c.HTTP.ListenAddr, ":")
	port := parts[len(parts)-1]
	for hostname, route := range c.HTTP.K8s.Routes {
		scheme := route.Scheme
		if scheme == "" {
			scheme = "http"
		}
		fmt.Printf("  %s://%s:%s -> %s:%d (%s/%s)\n", scheme, hostname, port, route.TargetDisplay(), route.Port, route.Context, route.Namespace)
	}
}

func (c *Config) PrintTCPRoutes() {
	if len(c.TCP.K8s.Routes) == 0 {
		return
	}
	fmt.Printf("TCP Routes (%d):\n", len(c.TCP.K8s.Routes))
	for localPort, route := range c.TCP.K8s.Routes {
		fmt.Printf("  :%d -> %s:%d (%s/%s)\n", localPort, route.TargetDisplay(), route.Port, route.Context, route.Namespace)
	}
}

func (c *Config) PrintJumpRoutes() {
	if len(c.TCP.K8s.Jump) == 0 {
		return
	}
	fmt.Printf("Jump Routes (%d):\n", len(c.TCP.K8s.Jump))
	for localPort, route := range c.TCP.K8s.Jump {
		fmt.Printf("  :%d via %s -> %s:%d (%s/%s) [%s]\n", localPort, route.Via.TargetDisplay(), route.Target.Host, route.Target.Port, route.Context, route.Namespace, route.GetMethod())
	}
}
