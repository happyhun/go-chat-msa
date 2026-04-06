//go:build e2e

package e2e_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/go-connections/nat"
	"github.com/stretchr/testify/suite"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mongodb"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
	"gopkg.in/yaml.v3"
)

type E2ESuite struct {
	suite.Suite
	ctx               context.Context
	net               *testcontainers.DockerNetwork
	postgres          *postgres.PostgresContainer
	mongo             *mongodb.MongoDBContainer
	userService       testcontainers.Container
	chatService       testcontainers.Container
	apiGateway        testcontainers.Container
	wsGateway         testcontainers.Container
	websocketService1 testcontainers.Container
	websocketService2 testcontainers.Container
	gatewayBaseURL    string
	wsBaseURL         string
}

func TestE2ESuite(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E test in short mode")
	}
	suite.Run(t, new(E2ESuite))
}

func (s *E2ESuite) SetupSuite() {
	setupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	s.ctx = context.Background()
	s.startCompose(setupCtx)
}

func (s *E2ESuite) TearDownSuite() {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	containers := []testcontainers.Container{
		s.websocketService1, s.websocketService2, s.apiGateway, s.wsGateway,
		s.chatService, s.userService, s.mongo, s.postgres,
	}

	for _, c := range containers {
		if c != nil {
			c.Terminate(cleanupCtx)
		}
	}
	if s.net != nil {
		s.net.Remove(cleanupCtx)
	}
}

func (s *E2ESuite) TearDownTest() {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.cleanupDatabases(cleanupCtx)
}

type ComposeConfig struct {
	Services map[string]ServiceConfig `yaml:"services"`
}

type ServiceConfig struct {
	Image       string                      `yaml:"image"`
	Environment []string                    `yaml:"environment"`
	Volumes     []string                    `yaml:"volumes"`
	Command     []string                    `yaml:"command"`
	Build       *BuildConfig                `yaml:"build"`
	Ports       []string                    `yaml:"ports"`
	Healthcheck *HealthcheckConfig          `yaml:"healthcheck"`
	DependsOn   map[string]DependencyConfig `yaml:"depends_on"`
}

type BuildConfig struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile"`
	Args       map[string]string `yaml:"args"`
}

type HealthcheckConfig struct {
	Test     []string `yaml:"test"`
	Interval string   `yaml:"interval"`
	Timeout  string   `yaml:"timeout"`
	Retries  int      `yaml:"retries"`
}

type DependencyConfig struct {
	Condition string `yaml:"condition"`
}

func (s *E2ESuite) startCompose(ctx context.Context) {
	composeFile, err := filepath.Abs("configs/docker-compose.test.yaml")
	s.Require().NoError(err)

	data, err := os.ReadFile(composeFile)
	s.Require().NoError(err)

	var config ComposeConfig
	err = yaml.Unmarshal(data, &config)
	s.Require().NoError(err)

	requiredServices := []string{
		"postgres", "mongo",
		"postgres-migrate", "mongo-migrate",
		"user-service", "chat-service", "api-gateway",
		"websocket-service-1", "websocket-service-2",
	}
	for _, svc := range requiredServices {
		_, ok := config.Services[svc]
		s.Require().True(ok, "필수 서비스 '%s'가 docker-compose.test.yaml에 정의되어 있지 않습니다.", svc)
	}

	configDir := filepath.Dir(composeFile)

	nt, err := network.New(ctx)
	s.Require().NoError(err)
	s.net = nt

	s.postgres = s.runPostgres(ctx, config.Services["postgres"])
	s.mongo = s.runMongo(ctx, config.Services["mongo"])

	_ = s.runService(ctx, "postgres-migrate", config.Services["postgres-migrate"], configDir)
	_ = s.runService(ctx, "mongo-migrate", config.Services["mongo-migrate"], configDir)

	s.userService = s.runService(ctx, "user-service", config.Services["user-service"], configDir)
	s.chatService = s.runService(ctx, "chat-service", config.Services["chat-service"], configDir)
	s.websocketService1 = s.runService(ctx, "websocket-service-1", config.Services["websocket-service-1"], configDir)
	s.websocketService2 = s.runService(ctx, "websocket-service-2", config.Services["websocket-service-2"], configDir)
	s.wsGateway = s.runService(ctx, "ws-gateway", config.Services["ws-gateway"], configDir)
	s.apiGateway = s.runService(ctx, "api-gateway", config.Services["api-gateway"], configDir)

	s.gatewayBaseURL = s.getEndpoint(ctx, s.apiGateway, "8080/tcp")
	s.wsBaseURL = s.getEndpoint(ctx, s.wsGateway, "8088/tcp")
}

func (s *E2ESuite) runPostgres(ctx context.Context, cfg ServiceConfig) *postgres.PostgresContainer {
	envMap := make(map[string]string)
	for _, env := range cfg.Environment {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	dbName, ok := envMap["POSTGRES_DB"]
	s.Require().True(ok, "POSTGRES_DB 환경 변수가 설정되어야 합니다.")
	user, ok := envMap["POSTGRES_USER"]
	s.Require().True(ok, "POSTGRES_USER 환경 변수가 설정되어야 합니다.")
	password := envMap["POSTGRES_PASSWORD"]

	opts := []testcontainers.ContainerCustomizer{
		postgres.WithDatabase(dbName),
		postgres.WithUsername(user),
		network.WithNetwork([]string{"postgres"}, s.net),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(30 * time.Second),
		),
	}

	if password != "" {
		opts = append(opts, postgres.WithPassword(password))
	}

	for k, v := range envMap {
		if k != "POSTGRES_DB" && k != "POSTGRES_USER" && k != "POSTGRES_PASSWORD" {
			opts = append(opts, testcontainers.WithEnv(map[string]string{k: v}))
		}
	}

	c, err := postgres.Run(ctx, cfg.Image, opts...)
	s.Require().NoError(err)
	return c
}

func (s *E2ESuite) runMongo(ctx context.Context, cfg ServiceConfig) *mongodb.MongoDBContainer {
	envMap := make(map[string]string)
	for _, env := range cfg.Environment {
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	c, err := mongodb.Run(ctx, cfg.Image,
		network.WithNetwork([]string{"mongo"}, s.net),
		testcontainers.WithEnv(envMap),
	)
	s.Require().NoError(err)
	return c
}

func (s *E2ESuite) runService(ctx context.Context, hostname string, cfg ServiceConfig, configDir string) testcontainers.Container {
	env := make(map[string]string)
	for _, e := range cfg.Environment {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			env[parts[0]] = parts[1]
		}
	}

	var mounts testcontainers.ContainerMounts
	for _, v := range cfg.Volumes {
		parts := strings.Split(v, ":")
		if len(parts) >= 2 {
			absPath := filepath.Join(configDir, parts[0])
			ro := false
			if len(parts) >= 3 && parts[2] == "ro" {
				ro = true
			}
			mounts = append(mounts, testcontainers.ContainerMount{
				Source:   testcontainers.GenericBindMountSource{HostPath: absPath},
				Target:   testcontainers.ContainerMountTarget(parts[1]),
				ReadOnly: ro,
			})
		}
	}

	buildArgs := make(map[string]*string)
	if cfg.Build != nil {
		for k, v := range cfg.Build.Args {
			val := v
			buildArgs[k] = &val
		}
	}

	imageTag := cfg.Image
	if cfg.Build != nil {
		imageTag = fmt.Sprintf("go-chat-msa/%s:e2e", hostname)
		if strings.Contains(hostname, "websocket-service") {
			imageTag = "go-chat-msa/websocket-service:e2e"
		}
	}

	if cfg.Build != nil {
		buildCtx := filepath.Join(configDir, cfg.Build.Context)
		s.T().Logf("[%s] 이미지 빌드 시작: %s", hostname, imageTag)
		err := s.buildDockerImage(ctx, buildCtx, cfg.Build.Dockerfile, imageTag, buildArgs)
		s.Require().NoError(err, "이미지 빌드 실패")
	}

	var waitStrategy wait.Strategy

	var exposedPorts []string
	if hostname == "api-gateway" {
		exposedPorts = append(exposedPorts, "8080/tcp")
		waitStrategy = wait.ForHTTP("/health").WithPort("8080/tcp")
	} else if hostname == "ws-gateway" {
		exposedPorts = append(exposedPorts, "8088/tcp")
		waitStrategy = wait.ForHTTP("/health").WithPort("8088/tcp")
	} else if strings.HasPrefix(hostname, "websocket-service") {
		exposedPorts = append(exposedPorts, "8081/tcp")
		waitStrategy = wait.ForHTTP("/health").WithPort("8081/tcp")
	} else if hostname == "user-service" {
		exposedPorts = append(exposedPorts, "50051/tcp")
		waitStrategy = wait.ForListeningPort("50051/tcp")
	} else if hostname == "chat-service" {
		exposedPorts = append(exposedPorts, "50052/tcp")
		waitStrategy = wait.ForListeningPort("50052/tcp")
	} else if strings.Contains(hostname, "migrate") {
		waitStrategy = wait.ForExit()
	} else {
		waitStrategy = wait.ForHealthCheck()
	}

	req := testcontainers.ContainerRequest{
		Image:          imageTag,
		Networks:       []string{s.net.Name},
		NetworkAliases: map[string][]string{s.net.Name: {hostname}},
		Env:            env,
		ExposedPorts:   exposedPorts,
		WaitingFor:     waitStrategy,
		Hostname:       hostname,
		Mounts:         mounts,
		Cmd:            cfg.Command,
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	s.Require().NoError(err, "컨테이너 시작 실패: %s", hostname)

	if strings.Contains(hostname, "migrate") {
		state, err := c.State(ctx)
		if err == nil && state.ExitCode != 0 {
			logs, _ := c.Logs(ctx)
			logBytes, _ := io.ReadAll(logs)
			if s.postgres != nil {
				pgLogs, _ := s.postgres.Logs(ctx)
				pgLogBytes, _ := io.ReadAll(pgLogs)
				s.T().Logf("Postgres Logs:\n%s", string(pgLogBytes))
			}
			s.FailNow(fmt.Sprintf("[%s] 마이그레이션 실패 (Exit Code %d):\n%s", hostname, state.ExitCode, string(logBytes)))
		}
	}

	return c
}

func (s *E2ESuite) buildDockerImage(ctx context.Context, buildCtx, dockerfile, tag string, args map[string]*string) error {
	cmdArgs := []string{"build", "-t", tag, "-f", filepath.Join(buildCtx, dockerfile)}
	for k, v := range args {
		if v != nil {
			cmdArgs = append(cmdArgs, "--build-arg", fmt.Sprintf("%s=%s", k, *v))
		}
	}
	cmdArgs = append(cmdArgs, buildCtx)

	cmd := exec.CommandContext(ctx, "docker", cmdArgs...)

	return cmd.Run()
}

func (s *E2ESuite) getEndpoint(ctx context.Context, c testcontainers.Container, port string) string {
	endpoint, err := c.PortEndpoint(ctx, nat.Port(port), "http")
	s.Require().NoError(err)
	return endpoint
}
