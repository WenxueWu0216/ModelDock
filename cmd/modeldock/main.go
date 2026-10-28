// Command modeldock is the ModelDock control plane.
//
//	modeldock serve      run the API, outbox relay and reconciler
//	modeldock consume    run the Kafka consumer that builds the health dashboard
//	modeldock migrate    apply database migrations
//	modeldock token      mint a dev JWT
//	modeldock render     print the Kubernetes manifests for a version (for kubeconform)
//
// Configuration is via flags, each defaulting to a MODELDOCK_* env var.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/wenxuewu0216/modeldock/api"
	"github.com/wenxuewu0216/modeldock/auth"
	"github.com/wenxuewu0216/modeldock/domain"
	"github.com/wenxuewu0216/modeldock/events"
	"github.com/wenxuewu0216/modeldock/kube"
	"github.com/wenxuewu0216/modeldock/service"
	"github.com/wenxuewu0216/modeldock/store"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, log, os.Args[2:])
	case "consume":
		err = consume(ctx, log, os.Args[2:])
	case "migrate":
		err = migrateCmd(os.Args[2:])
	case "token":
		err = token(os.Args[2:])
	case "render":
		err = render(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("modeldock failed", "cmd", os.Args[1], "err", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: modeldock {serve|consume|migrate|token|render} [flags]")
}

type commonFlags struct {
	dbURL    string
	secret   string
	issuer   string
	audience string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dbURL, "database-url", env("MODELDOCK_DATABASE_URL", "postgres://modeldock:modeldock@localhost:5432/modeldock?sslmode=disable"), "Postgres URL")
	fs.StringVar(&c.secret, "jwt-secret", env("MODELDOCK_JWT_SECRET", ""), "HS256 secret (>= 32 bytes)")
	fs.StringVar(&c.issuer, "jwt-issuer", env("MODELDOCK_JWT_ISSUER", "modeldock"), "expected JWT issuer")
	fs.StringVar(&c.audience, "jwt-audience", env("MODELDOCK_JWT_AUDIENCE", "modeldock-api"), "expected JWT audience")
}

func serve(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	var c commonFlags
	c.register(fs)
	addr := fs.String("addr", env("MODELDOCK_ADDR", ":8080"), "listen address")
	kubeMode := fs.String("kube", env("MODELDOCK_KUBE", "fake"), "kubernetes backend: fake | real")
	kubeconfig := fs.String("kubeconfig", env("MODELDOCK_KUBECONFIG", ""), "kubeconfig path (real mode; empty = in-cluster or $KUBECONFIG)")
	eventsMode := fs.String("events", env("MODELDOCK_EVENTS", "log"), "event sink: kafka | log")
	brokers := fs.String("kafka-brokers", env("MODELDOCK_KAFKA_BROKERS", "localhost:9092"), "comma-separated Kafka brokers")
	createTopics := fs.Bool("kafka-create-topics", env("MODELDOCK_KAFKA_CREATE_TOPICS", "true") == "true", "create topics on startup")
	timeout := fs.Duration("rollout-timeout", envDuration("MODELDOCK_ROLLOUT_TIMEOUT", 5*time.Minute), "mark a deployment failed if not ready within this")
	autoRollback := fs.Bool("auto-rollback", env("MODELDOCK_AUTO_ROLLBACK", "false") == "true", "roll back automatically when a rollout fails")
	interval := fs.Duration("reconcile-interval", envDuration("MODELDOCK_RECONCILE_INTERVAL", 2*time.Second), "reconciler poll interval")
	playground := fs.Bool("playground", env("MODELDOCK_PLAYGROUND", "true") == "true", "serve GraphQL playground at /playground")
	fakeReadyPolls := fs.Int("fake-ready-polls", 1, "fake kube: status polls before a rollout is ready")
	_ = fs.Parse(args)

	authn, err := auth.NewAuthenticator([]byte(c.secret), c.issuer, c.audience)
	if err != nil {
		return err
	}
	if err := store.Migrate(c.dbURL); err != nil {
		return err
	}
	st, err := store.Open(ctx, c.dbURL)
	if err != nil {
		return err
	}
	defer st.Close()

	var kc kube.Client
	switch *kubeMode {
	case "fake":
		f := kube.NewFake()
		f.ReadyAfterPolls = *fakeReadyPolls
		kc = f
		log.Warn("using in-memory fake Kubernetes client: nothing is deployed to a real cluster")
	case "real":
		cfg, err := kube.RESTConfig(*kubeconfig)
		if err != nil {
			return err
		}
		real, err := kube.NewClientGo(ctx, cfg, []string{"modeldock-staging", "modeldock-prod"})
		if err != nil {
			return err
		}
		defer real.Close()
		kc = real
	default:
		return fmt.Errorf("unknown -kube mode %q", *kubeMode)
	}

	var pub events.Publisher
	switch *eventsMode {
	case "kafka":
		bs := strings.Split(*brokers, ",")
		if *createTopics {
			if err := events.EnsureTopics(ctx, bs[0], 3, 1); err != nil {
				return err
			}
		}
		pub = events.NewKafkaPublisher(bs)
	case "log":
		pub = &events.MemoryPublisher{Log: log}
		log.Warn("events are logged, not sent to Kafka (-events=log)")
	default:
		return fmt.Errorf("unknown -events mode %q", *eventsMode)
	}
	defer pub.Close()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	events.RegisterMetrics(reg)
	svc := service.New(st, nil, service.NewMetrics(reg), *autoRollback)
	rec := &kube.Reconciler{Store: st, Kube: kc, Transitions: svc, Timeout: *timeout, Interval: *interval, Log: log}
	svc.Kicker = rec
	relay := &events.Relay{Source: st, Lock: st, Publisher: pub, Log: log}

	handler := api.NewHandler(api.Config{
		Service: svc, Auth: authn, Metrics: api.NewHTTPMetrics(reg), Gatherer: reg, Log: log,
		Ready: st.Ping, Playground: *playground,
	})
	srv := &http.Server{Addr: *addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second}

	go rec.Run(ctx)
	go relay.Run(ctx)
	errc := make(chan error, 1)
	go func() {
		log.Info("modeldock listening", "addr", *addr, "kube", *kubeMode, "events", *eventsMode, "auto_rollback", *autoRollback)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return fmt.Errorf("http shutdown: %w", err)
	}
	return nil
}

func consume(ctx context.Context, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("consume", flag.ExitOnError)
	dbURL := fs.String("database-url", env("MODELDOCK_DATABASE_URL", "postgres://modeldock:modeldock@localhost:5432/modeldock?sslmode=disable"), "Postgres URL")
	brokers := fs.String("kafka-brokers", env("MODELDOCK_KAFKA_BROKERS", "localhost:9092"), "comma-separated Kafka brokers")
	group := fs.String("group", env("MODELDOCK_CONSUMER_GROUP", "modeldock-dashboard"), "consumer group")
	_ = fs.Parse(args)
	if err := store.Migrate(*dbURL); err != nil {
		return err
	}
	st, err := store.Open(ctx, *dbURL)
	if err != nil {
		return err
	}
	defer st.Close()
	c := &events.DashboardConsumer{Brokers: strings.Split(*brokers, ","), GroupID: *group, Sink: st, Log: log}
	log.Info("dashboard consumer started", "brokers", *brokers, "group", *group)
	return c.Run(ctx)
}

func migrateCmd(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dbURL := fs.String("database-url", env("MODELDOCK_DATABASE_URL", ""), "Postgres URL")
	_ = fs.Parse(args)
	return store.Migrate(*dbURL)
}

func token(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	var c commonFlags
	c.register(fs)
	sub := fs.String("sub", "", "user ID")
	name := fs.String("name", "", "display name")
	role := fs.String("role", "viewer", "viewer | deployer | approver | admin")
	ttl := fs.Duration("ttl", 12*time.Hour, "token lifetime")
	_ = fs.Parse(args)
	if *sub == "" || !domain.Role(*role).Valid() {
		return errors.New("token: -sub and a valid -role are required")
	}
	a, err := auth.NewAuthenticator([]byte(c.secret), c.issuer, c.audience)
	if err != nil {
		return err
	}
	t, err := a.Issue(domain.User{ID: *sub, Name: *name, Role: domain.Role(*role)}, *ttl)
	if err != nil {
		return err
	}
	fmt.Println(t)
	return nil
}

func render(args []string) error {
	fs := flag.NewFlagSet("render", flag.ExitOnError)
	name := fs.String("model", "iris", "model name")
	version := fs.String("version", "1.0.0", "version")
	image := fs.String("image", "modeldock/iris-server:1.0.0", "image")
	envName := fs.String("env", "staging", "environment")
	_ = fs.Parse(args)
	e, err := domain.ParseEnvironment(*envName)
	if err != nil {
		return err
	}
	v := domain.ModelVersion{Version: *version, Image: *image}
	if err := v.Validate(); err != nil {
		return err
	}
	b, err := kube.RenderYAML(kube.WorkloadSpec{
		Namespace: "modeldock-" + string(e), Name: *name, Environment: e, Version: v.Version, Image: v.Image,
		Port: v.Port, Resources: v.Resources, Scaling: v.Scaling, Metadata: map[string]string{"source": "render"},
		DeploymentID: "00000000-0000-0000-0000-000000000000", Revision: 1, ProgressDeadlineSeconds: 300,
	})
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(b)
	return err
}
