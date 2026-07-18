package services

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sauraku/devops-control/internal/db"
	"github.com/sauraku/devops-control/internal/docker"
	"github.com/sauraku/devops-control/internal/models"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestProjectDeleteRefusesAnActiveOperationBeforeDockerMutation(t *testing.T) {
	db.InitCrypto(strings.Repeat("d", 64))
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	baseDir := t.TempDir()
	project := &models.Project{
		ID: "medusa", Name: "Medusa", BranchName: "main",
		AppDir: filepath.Join(baseDir, "Projects", "medusa"),
	}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	if err := database.CreateLock(&models.DeployLock{
		ProjectID: "medusa", OperationID: "deploy-medusa-1", Operation: "deploy",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	service := NewProjectService(database, docker.NewClient(), NewAuditService(database), &models.Config{BaseDir: baseDir})
	if err := service.Delete("medusa"); err == nil || !strings.Contains(err.Error(), "another operation is active") {
		t.Fatalf("delete during active operation returned %v", err)
	}
	if _, err := database.GetProject("medusa"); err != nil {
		t.Fatalf("project was removed despite active operation: %v", err)
	}
}

func TestTeardownEnvironmentUsesOverridesAndSafePlaceholders(t *testing.T) {
	db.InitCrypto(strings.Repeat("t", 64))
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	baseDir := t.TempDir()
	appDir := filepath.Join(baseDir, "Projects", "medusa")
	if err := os.MkdirAll(appDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, ".env.template"), []byte(`POSTGRES_PORT=
REDIS_URL=
COOKIE_SECURE=
JWT_SECRET=
NEXT_PUBLIC_MEDUSA_PUBLISHABLE_KEY=
PRESERVED=template
`), 0o600); err != nil {
		t.Fatal(err)
	}
	project := &models.Project{ID: "medusa", Name: "Medusa", BranchName: "main", AppDir: appDir}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	if err := database.SaveProjectEnvOverrides(project.ID, map[string]string{
		"PRESERVED":  "operator-value",
		"JWT_SECRET": "saved-secret",
	}); err != nil {
		t.Fatal(err)
	}

	service := NewProjectService(database, docker.NewClient(), NewAuditService(database), &models.Config{BaseDir: baseDir})
	values, err := service.teardownEnvironment(project, "medusa-main")
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range values {
		if value == "" {
			t.Fatalf("teardown environment left %s empty", key)
		}
	}
	if got := values["POSTGRES_PORT"]; got != "1" {
		t.Fatalf("POSTGRES_PORT placeholder = %q", got)
	}
	if got := values["REDIS_URL"]; got != "http://teardown.invalid" {
		t.Fatalf("REDIS_URL placeholder = %q", got)
	}
	if got := values["COOKIE_SECURE"]; got != "false" {
		t.Fatalf("COOKIE_SECURE placeholder = %q", got)
	}
	if got := values["JWT_SECRET"]; got != "saved-secret" {
		t.Fatalf("saved JWT_SECRET was not preserved")
	}
	if got := values["PRESERVED"]; got != "operator-value" {
		t.Fatalf("saved override = %q", got)
	}
	if got := values["COMPOSE_PROJECT_NAME"]; got != "medusa-main" {
		t.Fatalf("COMPOSE_PROJECT_NAME = %q", got)
	}
	if got := values["ENV_NAME"]; got != "main" {
		t.Fatalf("ENV_NAME = %q", got)
	}
}

func TestProjectAndBranchSlugs(t *testing.T) {
	service := &ProjectService{}
	if got := service.SlugID(" Medusa Store "); got != "medusa-store" {
		t.Fatalf("SlugID = %q", got)
	}
	if got := branchSlug("refs/heads/Feature/Checkout"); got != "refs-heads-feature-checkout" {
		t.Fatalf("branchSlug = %q", got)
	}
	if got := normalizeRef("refs/heads/Feature/Checkout"); got != "Feature/Checkout" {
		t.Fatalf("normalizeRef = %q", got)
	}
}

func TestReadEnvTemplateUsesProjectEnvironmentContract(t *testing.T) {
	db.InitCrypto(strings.Repeat("e", 64))
	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })

	appDir := t.TempDir()
	project := &models.Project{ID: "medusa", Name: "Medusa", BranchName: "main", AppDir: appDir}
	if err := database.UpsertProject(project); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, ".env.template"), []byte(`NEXT_PUBLIC_MEDUSA_PUBLISHABLE_KEY=
POSTGRES_PASSWORD=
OPENSEARCH_ADMIN_PASSWORD_ROTATION_CONFIRMED=false
SMTP_HOST=
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "devops.json"), []byte(`{
  "environment": {
    "operator_required": ["NEXT_PUBLIC_MEDUSA_PUBLISHABLE_KEY"],
    "generated_secrets": ["POSTGRES_PASSWORD"],
    "non_secret": ["OPENSEARCH_ADMIN_PASSWORD_ROTATION_CONFIRMED"]
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}

	service := NewProjectService(database, docker.NewClient(), NewAuditService(database), &models.Config{BaseDir: t.TempDir()})
	vars, _, err := service.ReadEnvTemplate(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	byKey := make(map[string]EnvVar, len(vars))
	for _, variable := range vars {
		byKey[variable.Key] = variable
	}
	if !byKey["NEXT_PUBLIC_MEDUSA_PUBLISHABLE_KEY"].OperatorRequired {
		t.Fatal("publishable key was not marked operator-required")
	}
	if got := byKey["POSTGRES_PASSWORD"]; !got.ControllerManaged || got.OperatorRequired || !got.IsSecret {
		t.Fatalf("generated password metadata = %+v", got)
	}
	if got := byKey["OPENSEARCH_ADMIN_PASSWORD_ROTATION_CONFIRMED"]; got.IsSecret || got.Default != "false" {
		t.Fatalf("rotation confirmation was masked as a secret: %+v", got)
	}
	if got := byKey["SMTP_HOST"]; got.OperatorRequired || got.ControllerManaged {
		t.Fatalf("optional SMTP variable metadata = %+v", got)
	}
}

func TestProjectScriptUsesBusyBoxCompatibleMktempTemplates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "project.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)

	// The production controller image is Alpine-based and therefore uses
	// BusyBox mktemp, which accepts templates only when the X sequence is at
	// the end of the pathname.
	if !strings.Contains(script, `mktemp "${STATE_DIR}/compose-config.XXXXXX"`) {
		t.Fatal("project deploy script does not use a BusyBox-compatible compose-config mktemp template")
	}
	if strings.Contains(script, "compose-config.XXXXXX.json") {
		t.Fatal("project deploy script appends an extension after the mktemp X template")
	}
}

func TestProjectAppDirMustStayInsideProjectsRoot(t *testing.T) {
	baseDir := t.TempDir()
	projectsDir := filepath.Join(baseDir, "Projects")
	if err := os.MkdirAll(projectsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(projectsDir, "custom-medusa")
	if got, err := validateProjectAppDir(baseDir, valid); err != nil || got != valid {
		t.Fatalf("valid app_dir = %q, %v", got, err)
	}
	for _, unsafe := range []string{projectsDir, filepath.Join(projectsDir, "..", "outside")} {
		if _, err := validateProjectAppDir(baseDir, unsafe); err == nil {
			t.Fatalf("unsafe app_dir accepted: %s", unsafe)
		}
	}

	escape := filepath.Join(projectsDir, "escape")
	if err := os.Symlink(t.TempDir(), escape); err != nil {
		t.Fatal(err)
	}
	if _, err := validateProjectAppDir(baseDir, filepath.Join(escape, "medusa")); err == nil {
		t.Fatal("app_dir through escaping symlink was accepted")
	}
}

func TestRepoURLValidation(t *testing.T) {
	service := &ProjectService{cfg: &models.Config{AllowedRepoPrefixes: []string{"https://github.com/sauraku/"}}}
	if err := service.validateRepoURL("https://github.com/sauraku/medusa.git"); err != nil {
		t.Fatalf("valid repository rejected: %v", err)
	}
	for _, raw := range []string{
		"https://github.com.evil/sauraku/medusa",
		"https://github.com/sauraku/medusa/extra",
		"https://user@github.com/sauraku/medusa",
		"https://github.com/sauraku-other/medusa",
	} {
		if err := service.validateRepoURL(raw); err == nil {
			t.Errorf("unsafe repository URL accepted: %s", raw)
		}
	}
}

func TestRunnerRegistrationTokenExchangesPATInController(t *testing.T) {
	const pat = "github" + "_pat_controller_only"
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/repos/sauraku/medusa/actions/runners/registration-token" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+pat {
			t.Errorf("authorization header = %q", got)
		}
		if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
			t.Errorf("GitHub API version = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusCreated,
			Status:     "201 Created",
			Body:       io.NopCloser(strings.NewReader(`{"token":"short-lived-registration-token"}`)),
			Header:     make(http.Header),
		}, nil
	})}

	service := &ProjectService{
		cfg:              &models.Config{AllowedRepoPrefixes: []string{"https://github.com/sauraku/"}},
		githubAPIBaseURL: "https://api.github.test",
		githubHTTPClient: client,
	}
	token, err := service.runnerRegistrationToken(context.Background(), "https://github.com/sauraku/medusa.git", pat)
	if err != nil {
		t.Fatal(err)
	}
	if token != "short-lived-registration-token" {
		t.Fatalf("registration token = %q", token)
	}
}

func TestRunnerRegistrationTokenPreservesDirectRegistrationToken(t *testing.T) {
	service := &ProjectService{cfg: &models.Config{}}
	token, err := service.runnerRegistrationToken(context.Background(), "https://github.com/sauraku/medusa", "direct-registration-token")
	if err != nil {
		t.Fatal(err)
	}
	if token != "direct-registration-token" {
		t.Fatalf("registration token = %q", token)
	}
}

func TestRunnerCredentialClassificationIncludesLegacyPATs(t *testing.T) {
	legacyClassicPAT := "gh" + "p_classic_pat"
	for _, token := range []string{
		legacyClassicPAT,
		"github" + "_pat_modern_fine_grained_pat",
		"0123456789abcdef0123456789abcdef01234567",
	} {
		if !isGitHubPersonalAccessToken(token) {
			t.Errorf("PAT was not recognized: %s", token)
		}
	}
	if isGitHubPersonalAccessToken("short-lived-registration-token") {
		t.Fatal("registration token was classified as a PAT")
	}
}

func TestRunnerComposeHasNoRegistrationCredentialEnvironment(t *testing.T) {
	composePath := filepath.Join("..", "..", "deploy", "runner", "docker-compose.runner.yml")
	data, err := os.ReadFile(composePath)
	if err != nil {
		t.Fatal(err)
	}
	compose := string(data)
	if strings.Contains(compose, "RUNNER_TOKEN") {
		t.Fatal("runner credential is present in Compose environment/config")
	}
	if !strings.Contains(compose, "/run/devops-runner-registration") || !strings.Contains(compose, "tmpfs:") {
		t.Fatal("runner registration handoff is not backed by tmpfs")
	}
}

func TestRunnerRegistrationTokenErrorDoesNotExposeCredentialOrResponse(t *testing.T) {
	const pat = "gh" + "p_not_appear_in_error"
	client := &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Status:     "403 Forbidden",
			Body:       io.NopCloser(strings.NewReader(`{"message":"sensitive response body"}`)),
			Header:     make(http.Header),
		}, nil
	})}
	service := &ProjectService{
		cfg:              &models.Config{AllowedRepoPrefixes: []string{"https://github.com/"}},
		githubAPIBaseURL: "https://api.github.test",
		githubHTTPClient: client,
	}
	_, err := service.runnerRegistrationToken(context.Background(), "https://github.com/sauraku/medusa", pat)
	if err == nil {
		t.Fatal("GitHub API error was accepted")
	}
	if strings.Contains(err.Error(), pat) || strings.Contains(err.Error(), "sensitive response body") {
		t.Fatalf("error exposes sensitive data: %v", err)
	}
}

func TestRunnerEntrypointConsumesRegistrationFileBeforeConfig(t *testing.T) {
	runnerDir := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "token")
	captureFile := filepath.Join(t.TempDir(), "config-args")
	const registrationToken = "short-lived-registration-token"
	if err := os.WriteFile(tokenFile, []byte(registrationToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runnerDir, ".runner_version"), []byte("test-version"), 0o600); err != nil {
		t.Fatal(err)
	}
	configScript := `#!/bin/bash
set -euo pipefail
[ ! -e "$RUNNER_REGISTRATION_TOKEN_FILE" ]
printf '%s\n' "$@" > "$RUNNER_CONFIG_CAPTURE"
`
	runScript := "#!/bin/bash\necho 'Listening for Jobs'\n"
	for name, content := range map[string]string{"config.sh": configScript, "run.sh": runScript} {
		path := filepath.Join(runnerDir, name)
		if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	entrypoint := filepath.Join("..", "..", "deploy", "runner", "entrypoint.sh")
	cmd := exec.Command("/bin/bash", entrypoint)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"RUNNER_DIR=" + runnerDir,
		"RUNNER_VERSION=test-version",
		"REPO_URL=https://github.com/sauraku/medusa",
		"RUNNER_REGISTRATION_TOKEN_FILE=" + tokenFile,
		"RUNNER_CONFIG_CAPTURE=" + captureFile,
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("entrypoint failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("registration token file still exists: %v", err)
	}
	args, err := os.ReadFile(captureFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), registrationToken) {
		t.Fatalf("config did not receive registration token: %s", args)
	}
}

func TestRunnerEntrypointRejectsPATFile(t *testing.T) {
	runnerDir := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "token")
	const pat = "gh" + "p_lived_credential"
	if err := os.WriteFile(tokenFile, []byte(pat), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runnerDir, ".runner_version"), []byte("test-version"), 0o600); err != nil {
		t.Fatal(err)
	}
	entrypoint := filepath.Join("..", "..", "deploy", "runner", "entrypoint.sh")
	cmd := exec.Command("/bin/bash", entrypoint)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"RUNNER_DIR=" + runnerDir,
		"RUNNER_VERSION=test-version",
		"REPO_URL=https://github.com/sauraku/medusa",
		"RUNNER_REGISTRATION_TOKEN_FILE=" + tokenFile,
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("entrypoint accepted a GitHub PAT")
	}
	if strings.Contains(string(output), pat) {
		t.Fatalf("entrypoint exposed PAT in output: %s", output)
	}
	if _, err := os.Stat(tokenFile); !os.IsNotExist(err) {
		t.Fatalf("rejected PAT file still exists: %v", err)
	}
}

func TestRestoreCommandMustBeArgumentArray(t *testing.T) {
	dir := t.TempDir()
	valid := `{"services":{"backend":{"restore":{"command":["npx","medusa","db:migrate"]}}}}`
	if err := os.WriteFile(filepath.Join(dir, "devops.json"), []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := restoreCommandJSON(dir)
	if err != nil || got != `["npx","medusa","db:migrate"]` {
		t.Fatalf("restore command = %q, %v", got, err)
	}
	invalid := strings.Replace(valid, `["npx","medusa","db:migrate"]`, `"npx medusa db:migrate"`, 1)
	if err := os.WriteFile(filepath.Join(dir, "devops.json"), []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := restoreCommandJSON(dir); err == nil {
		t.Fatal("shell command string was accepted")
	}
}

func TestComposeServicesPrefersMonitoringContract(t *testing.T) {
	dir := t.TempDir()
	config := `{"services":{"storefront":{},"backend":{},"opensearch":{}}}`
	if err := os.WriteFile(filepath.Join(dir, "devops.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	service := &ProjectService{}
	got := service.composeServices(&models.Project{AppDir: dir})
	want := []string{"backend", "opensearch", "storefront"}
	if len(got) != len(want) {
		t.Fatalf("composeServices = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("composeServices = %v, want %v", got, want)
		}
	}
}
