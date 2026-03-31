package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Config struct {
	Port    int
	DataDir string
	GitHub  GitHubConfig
	LLM     LLMConfig
	Server  ServerConfig
	Git     GitConfig
}

type GitHubConfig struct {
	Token               string
	WebhookSecret       string
	WebhookURL          string
	APIBaseURL          string
	ReviewCommentMarker string
}

type LLMConfig struct {
	APIBaseURL string
	APIKey     string
	Model      string
}

type ServerConfig struct {
	WorkerCount int
	QueueSize   int
}

type GitConfig struct {
	TempDir   string
	UserName  string
	UserEmail string
}

func Load() Config {
	loadEnvFile(".env")

	port := 8787
	if value := getEnv("PORT", "8787"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			port = parsed
		}
	}

	workerCount := 4
	if value := getEnv("WORKER_COUNT", "4"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			workerCount = parsed
		}
	}

	queueSize := 64
	if value := getEnv("QUEUE_SIZE", "64"); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			queueSize = parsed
		}
	}

	return Config{
		Port:    port,
		DataDir: filepath.Join(".", "data"),
		GitHub: GitHubConfig{
			Token:               getEnv("GITHUB_TOKEN", ""),
			WebhookSecret:       getEnv("GITHUB_WEBHOOK_SECRET", ""),
			WebhookURL:          strings.TrimSpace(getEnv("GITHUB_WEBHOOK_URL", "")),
			APIBaseURL:          strings.TrimRight(getEnv("GITHUB_API_BASE_URL", "https://api.github.com"), "/"),
			ReviewCommentMarker: getEnv("REVIEW_COMMENT_MARKER", "<!-- pr-agent-go-review -->"),
		},
		LLM: LLMConfig{
			APIBaseURL: strings.TrimRight(getEnv("OPENAI_API_BASE_URL", "https://api.openai.com/v1"), "/"),
			APIKey:     getEnv("OPENAI_API_KEY", ""),
			Model:      getEnv("OPENAI_MODEL", "gpt-4.1-mini"),
		},
		Server: ServerConfig{
			WorkerCount: workerCount,
			QueueSize:   queueSize,
		},
		Git: GitConfig{
			TempDir:   getEnv("GIT_TEMP_DIR", filepath.Join(os.TempDir(), "pr-agent-go")),
			UserName:  getEnv("GIT_USER_NAME", "pr-agent-go"),
			UserEmail: getEnv("GIT_USER_EMAIL", "pr-agent-go@local"),
		},
	}
}

func loadEnvFile(filename string) {
	file, err := os.Open(filename)
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return strings.TrimSpace(value)
	}
	return fallback
}
