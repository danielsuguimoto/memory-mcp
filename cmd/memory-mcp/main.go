package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"memory-mcp/internal/auth"
	"memory-mcp/internal/config"
	"memory-mcp/internal/gitstore"
	"memory-mcp/internal/server"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	gitstore.ConfigureHTTP()
	authManager := auth.New(cfg)
	store := gitstore.New(cfg, authManager.Credentials)
	s := server.New(store, authManager)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.MCP.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "memory server stopped")
		os.Exit(1)
	}
}
