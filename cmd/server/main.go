package main

import (
	"log"
	"os"

	"pr-agent-go/internal/config"
	"pr-agent-go/internal/storage"
)

func main() {
	cfg := config.Load()

	store, err := storage.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("init storage: %v", err)
	}

	switch command := resolveCommand(os.Args[1:]); command {
	case "serve":
		if err := runServer(cfg, store); err != nil {
			log.Fatal(err)
		}
	case "status":
		if err := runStatus(cfg, store, hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "stats":
		if err := runStats(cfg, store, hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "doctor":
		if err := runDoctor(cfg, hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "logs":
		if err := runLogs(cfg, store, hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "review":
		if err := runReviewPR(cfg, store, os.Args[1:], hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "check":
		if err := runIntervenePR(cfg, store, os.Args[1:], hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "recheck":
		if err := runRecheckConflict(cfg, store, os.Args[1:], hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	case "add":
		if err := runRegisterWebhook(cfg, os.Args[1:], hasJSONFlag(os.Args[1:])); err != nil {
			log.Fatal(err)
		}
	default:
		printUsage()
		os.Exit(1)
	}
}

func resolveCommand(args []string) string {
	if len(args) == 0 {
		return "serve"
	}
	switch args[0] {
	case "serve", "status", "stats", "doctor", "logs", "review", "check", "recheck", "add":
		return args[0]
	case "review-pr":
		return "review"
	case "intervene-pr":
		return "check"
	case "register-webhook":
		return "add"
	default:
		return args[0]
	}
}

func hasJSONFlag(args []string) bool {
	for _, arg := range args {
		if arg == "--json" {
			return true
		}
	}
	return false
}
