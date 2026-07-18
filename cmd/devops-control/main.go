package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sauraku/devops-control/internal/api"
	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/docker"
	"github.com/sauraku/devops-control/internal/models"
	"github.com/sauraku/devops-control/internal/services"
)

func main() {
	cfg := loadConfig()

	if len(cfg.Token) < 32 {
		log.Fatal("DEPLOY_CONTROL_TOKEN must be at least 32 characters; deploy-control refuses to start with weak auth")
	}

	os.MkdirAll(cfg.DataDir, 0o750)
	os.MkdirAll(filepath.Join(cfg.BaseDir, "Logs"), 0o750)
	os.MkdirAll(filepath.Join(cfg.BaseDir, "Projects"), 0o750)
	if err := os.MkdirAll(cfg.RunDir, 0o750); err != nil {
		log.Fatalf("Failed to create controller runtime directory: %v", err)
	}
	if err := os.Chmod(cfg.RunDir, 0o750); err != nil {
		log.Fatalf("Failed to secure controller runtime directory: %v", err)
	}

	encKey := getEnv("ENCRYPTION_KEY", "")
	if len(encKey) < 32 {
		log.Fatal("ENCRYPTION_KEY must be at least 32 characters for encrypting stored credentials and project environment overrides")
	}
	db.InitCrypto(encKey)

	database, err := db.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()
	if err := database.MigrateLegacyRunnerTokens(); err != nil {
		log.Fatalf("migrate legacy runner credentials: %v", err)
	}
	if err := database.MigrateLegacyProjectEnvOverrides(); err != nil {
		log.Fatalf("migrate legacy project environment overrides: %v", err)
	}

	dockerClient := docker.NewClient()

	auditService := services.NewAuditService(database)
	projectService := services.NewProjectService(database, dockerClient, auditService, cfg)
	deployService := services.NewDeployService(database, dockerClient, auditService, cfg)
	backupService := services.NewBackupService(database, dockerClient, auditService, cfg)

	authenticator := api.NewAuthenticator(cfg.Token, cfg.CookieSecret, cfg.CookieSecure)
	handler := api.NewHandler(projectService, deployService, backupService, auditService, authenticator, cfg)
	router := api.NewRouter(handler, authenticator, cfg)
	deployService.ReconcileLocks()

	// The bootstrap credential is the default GHCR credential. Docker stores the
	// resulting auth config only in the controller's tmpfs DOCKER_CONFIG.
	if cfg.GithubToken != "" {
		if ok, message := dockerClient.RegistryLogin("ghcr.io", cfg.GithubUser, cfg.GithubToken); !ok {
			log.Printf("default GHCR login failed; projects with private images must provide registry credentials: %s", message)
		}

		// Seed GITHUB_TOKEN as runner token for the default project if provided.
		services.SafeGo("seedGithubToken", func() { projectService.SeedGithubToken(cfg.DefaultProjectID, cfg.GithubToken) })
	}

	// Reconcile project containers on startup — restart any that are stopped
	services.SafeGo("reconcileContainers", func() { projectService.ReconcileContainers() })

	// Start scheduled backups
	services.SafeGo("backupScheduler", func() { backupService.StartScheduler() })

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	unixServer := &http.Server{
		Handler:           router,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	if err := os.Remove(cfg.SocketPath); err != nil && !os.IsNotExist(err) {
		log.Fatalf("remove stale control socket: %v", err)
	}
	unixListener, err := net.Listen("unix", cfg.SocketPath)
	if err != nil {
		log.Fatalf("listen on control socket: %v", err)
	}
	if err := os.Chmod(cfg.SocketPath, 0o600); err != nil {
		unixListener.Close()
		log.Fatalf("set control socket permissions: %v", err)
	}
	defer os.Remove(cfg.SocketPath)

	log.Printf("Deploy Control listening on %s and unix://%s", addr, cfg.SocketPath)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		_ = unixServer.Shutdown(ctx)
		log.Println("Cleanup done.")
	}()

	go func() {
		if err := unixServer.Serve(unixListener); err != nil && err != http.ErrServerClosed {
			log.Printf("Unix socket server error: %v", err)
			_ = server.Close()
		}
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func loadConfig() *models.Config {
	cfg := &models.Config{
		Token:            getEnv("DEPLOY_CONTROL_TOKEN", ""),
		Host:             getEnv("DEPLOY_CONTROL_HOST", "127.0.0.1"),
		Port:             getEnvInt("DEPLOY_CONTROL_PORT", 8787),
		CookieSecret:     getEnv("COOKIE_SECRET", "dev-cookie-secret-change-me"),
		DefaultProjectID: getEnv("DEFAULT_PROJECT_ID", ""),
		BackupSchedule:   getEnv("BACKUP_SCHEDULE", "06:00"),
		BackupRetention:  getEnvInt("BACKUP_RETENTION", 30),
		EnableRestore:    getEnvBool("ENABLE_RESTORE", false),
		GithubToken:      getEnv("GITHUB_TOKEN", ""),
		GithubUser:       getEnv("GITHUB_USER", "sauraku"),
		RunnerImage:      getEnv("RUNNER_IMAGE", "ghcr.io/sauraku/devops-runner:main"),
		RunnerNetwork:    getEnv("RUNNER_NETWORK", "devops-control-runners"),
		CookieSecure:     getEnvBool("COOKIE_SECURE", false),
		EnableTerminal:   getEnvBool("ENABLE_TERMINAL", false),
		SSHKnownHosts:    getEnv("SSH_KNOWN_HOSTS", ""),
	}
	cfg.RunnerControlURL = strings.TrimRight(getEnv("RUNNER_CONTROL_URL", fmt.Sprintf("http://host.docker.internal:%d", cfg.Port)), "/")
	runnerURL, err := url.Parse(cfg.RunnerControlURL)
	if err != nil || runnerURL.Host == "" || (runnerURL.Scheme != "http" && runnerURL.Scheme != "https") || runnerURL.User != nil || runnerURL.RawQuery != "" || runnerURL.Fragment != "" {
		log.Fatal("RUNNER_CONTROL_URL must be an http(s) origin without credentials, query, or fragment")
	}

	if cfg.CookieSecret == "dev-cookie-secret-change-me" || len(cfg.CookieSecret) < 32 {
		log.Fatal("COOKIE_SECRET must be changed from the default. Set a unique secret via environment variable.")
	}

	baseDir := getEnv("BASE_DIR", "")
	if baseDir == "" {
		execPath, err := os.Executable()
		if err == nil {
			baseDir = filepath.Dir(filepath.Dir(execPath))
		} else {
			baseDir = "/opt/devops-control"
		}
	}
	cfg.BaseDir = baseDir
	cfg.DataDir = filepath.Join(baseDir, "State")
	cfg.RunDir = filepath.Join(baseDir, "Run")
	cfg.SocketPath = filepath.Join(cfg.RunDir, "devops-control.sock")

	// ProjectRoot is where scripts (deploy.sh, docker-compose.runner.yml) live
	exe, _ := os.Executable()
	cfg.ProjectRoot = filepath.Dir(exe)

	prefixes := getEnv("DEPLOY_CONTROL_ALLOWED_REPO_PREFIXES", "")
	if prefixes != "" {
		cfg.AllowedRepoPrefixes = strings.Split(prefixes, ",")
	} else {
		cfg.AllowedRepoPrefixes = []string{"https://github.com/"}
	}

	return cfg
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	if val := os.Getenv(key); val != "" {
		var n int
		if _, err := fmt.Sscanf(val, "%d", &n); err == nil {
			return n
		}
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal
	}
	return strings.ToLower(val) == "true" || val == "1"
}
