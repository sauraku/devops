package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
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

	if cfg.Token == "" {
		log.Fatal("DEPLOY_CONTROL_TOKEN is required; deploy-control refuses to start without auth.")
	}

	os.MkdirAll(cfg.DataDir, 0o750)
	os.MkdirAll(filepath.Join(cfg.BaseDir, "Logs"), 0o750)
	os.MkdirAll(filepath.Join(cfg.BaseDir, "Projects"), 0o750)

	database, err := db.Open(cfg.DataDir)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer database.Close()

	dockerClient := docker.NewClient()

	// Release any stale locks from a previous process
	if err := database.ReleaseAllLocks(); err != nil {
		log.Printf("Warning: failed to release stale locks: %v", err)
	}
	// Also clean up file-based lock directories used by deploy scripts
	if entries, err := os.ReadDir(cfg.DataDir); err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				lockDir := filepath.Join(cfg.DataDir, entry.Name(), "deploy-lock")
				if _, err := os.Stat(lockDir); err == nil {
					log.Printf("Cleaning up stale lock: %s", lockDir)
					os.RemoveAll(lockDir)
				}
			}
		}
	}

	auditService := services.NewAuditService(database)
	projectService := services.NewProjectService(database, dockerClient, auditService, cfg)
	deployService := services.NewDeployService(database, dockerClient, auditService, cfg)
	backupService := services.NewBackupService(database, dockerClient, auditService, cfg)

	authenticator := api.NewAuthenticator(cfg.Token, cfg.JWTSecret, cfg.CookieSecret, cfg.CookieSecure)
	encKey := getEnv("ENCRYPTION_KEY", "")
	if encKey == "" {
		log.Fatal("ENCRYPTION_KEY is required for encrypting registry passwords")
	}
	db.InitCrypto(encKey)
	handler := api.NewHandler(projectService, deployService, backupService, auditService, authenticator, cfg)
	router := api.NewRouter(handler, authenticator, cfg)

	// Seed GITHUB_TOKEN as runner token for default project if provided
	if cfg.GithubToken != "" {
		services.SafeGo("seedGithubToken", func() { projectService.SeedGithubToken(cfg.DefaultProjectID, cfg.GithubToken) })
	}

	// Reconcile project containers on startup — restart any that are stopped
	services.SafeGo("reconcileContainers", func() { projectService.ReconcileContainers() })

	// Start scheduled backups
	services.SafeGo("backupScheduler", func() { backupService.StartScheduler() })

	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
	log.Printf("Deploy Control listening on %s", addr)

	server := &http.Server{
		Addr:              addr,
		Handler:           router,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Println("Shutting down...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		server.Shutdown(ctx)
		projectService.StopAllRunners()
		database.ReleaseAllLocks()
		log.Println("Cleanup done.")
	}()

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}

func loadConfig() *models.Config {
	cfg := &models.Config{
		Token:          getEnv("DEPLOY_CONTROL_TOKEN", ""),
		Host:           getEnv("DEPLOY_CONTROL_HOST", "127.0.0.1"),
		Port:           getEnvInt("DEPLOY_CONTROL_PORT", 8787),
		JWTSecret:      getEnv("JWT_SECRET", "dev-jwt-secret-change-me"),
		CookieSecret:   getEnv("COOKIE_SECRET", "dev-cookie-secret-change-me"),
		DefaultProjectID: getEnv("DEFAULT_PROJECT_ID", ""),
		BackupSchedule: getEnv("BACKUP_SCHEDULE", "06:00"),
		BackupRetention: getEnvInt("BACKUP_RETENTION", 30),
		EnableRestore:   getEnvBool("ENABLE_RESTORE", false),
		GithubToken:    getEnv("GITHUB_TOKEN", ""),
		CookieSecure:   getEnvBool("COOKIE_SECURE", false),
	}

	if cfg.JWTSecret == "dev-jwt-secret-change-me" {
		log.Fatal("JWT_SECRET must be changed from the default. Set a unique secret via environment variable.")
	}
	if cfg.CookieSecret == "dev-cookie-secret-change-me" {
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
