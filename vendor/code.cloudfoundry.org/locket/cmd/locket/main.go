package main

import (
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"time"

	loggingclient "code.cloudfoundry.org/diego-logging-client"
	"code.cloudfoundry.org/go-loggregator/runtimeemitter"
	"github.com/go-sql-driver/mysql"
	"github.com/hashicorp/consul/api"
	"github.com/lib/pq"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"

	"code.cloudfoundry.org/bbs/db/sqldb/helpers"
	"code.cloudfoundry.org/bbs/guidprovider"
	"code.cloudfoundry.org/cfhttp"
	"code.cloudfoundry.org/clock"
	"code.cloudfoundry.org/consuladapter"
	"code.cloudfoundry.org/debugserver"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerflags"
	"code.cloudfoundry.org/locket"
	"code.cloudfoundry.org/locket/cmd/locket/config"
	"code.cloudfoundry.org/locket/db"
	"code.cloudfoundry.org/locket/expiration"
	"code.cloudfoundry.org/locket/grpcserver"
	"code.cloudfoundry.org/locket/handlers"
	"code.cloudfoundry.org/locket/metrics"
)

var configFilePath = flag.String(
	"config",
	"",
	"Path to Locket JSON Configuration file",
)

func main() {
	flag.Parse()

	cfg, err := config.NewLocketConfig(*configFilePath)
	if err != nil {
		panic("invalid-config-file: " + err.Error())
	}

	logger, reconfigurableSink := lagerflags.NewFromConfig("locket", cfg.LagerConfig)

	metronClient, err := initializeMetron(logger, cfg)
	if err != nil {
		logger.Error("failed-to-initialize-metron-client", err)
		os.Exit(1)
	}

	clock := clock.NewClock()

	connectionString := appendExtraConnectionStringParam(
		logger,
		cfg.DatabaseDriver,
		cfg.DatabaseConnectionString,
		cfg.SQLCACertFile,
	)

	sqlConn, err := sql.Open(cfg.DatabaseDriver, connectionString)
	if err != nil {
		logger.Fatal("failed-to-open-sql", err)
	}
	defer sqlConn.Close()

	sqlConn.SetMaxIdleConns(cfg.MaxOpenDatabaseConnections)
	sqlConn.SetMaxOpenConns(cfg.MaxOpenDatabaseConnections)

	err = sqlConn.Ping()
	if err != nil {
		logger.Fatal("sql-failed-to-connect", err)
	}

	dbMonitor := helpers.NewQueryMonitor()
	monitoredDB := helpers.NewMonitoredDB(sqlConn, dbMonitor)

	sqlDB := db.NewSQLDB(
		monitoredDB,
		cfg.DatabaseDriver,
		guidprovider.DefaultGuidProvider,
	)

	err = sqlDB.CreateLockTable(logger)
	if err != nil {
		logger.Fatal("failed-to-create-lock-table", err)
	}

	_, portString, err := net.SplitHostPort(cfg.ListenAddress)
	if err != nil {
		logger.Fatal("failed-invalid-listen-address", err)
	}

	portNum, err := net.LookupPort("tcp", portString)
	if err != nil {
		logger.Fatal("failed-invalid-listen-port", err)
	}

	tlsConfig, err := cfhttp.NewTLSConfig(cfg.CertFile, cfg.KeyFile, cfg.CaFile)
	if err != nil {
		logger.Fatal("invalid-tls-config", err)
	}

	lockMetricsNotifier := metrics.NewLockMetricsNotifier(logger, clock, metronClient, time.Duration(cfg.ReportInterval), sqlDB)
	dbMetricsNotifier := metrics.NewDBMetricsNotifier(logger, clock, metronClient, time.Duration(cfg.ReportInterval), sqlDB, dbMonitor)
	requestNotifier := metrics.NewRequestMetricsNotifier(logger, clock, metronClient, time.Duration(cfg.ReportInterval))
	lockPick := expiration.NewLockPick(sqlDB, clock, metronClient)
	burglar := expiration.NewBurglar(logger, sqlDB, lockPick, clock, locket.RetryInterval, metronClient)
	exitCh := make(chan struct{})
	handler := handlers.NewLocketHandler(logger, sqlDB, lockPick, requestNotifier, exitCh)
	server := grpcserver.NewGRPCServer(logger, cfg.ListenAddress, tlsConfig, handler)

	members := grouper.Members{
		{"server", server},
		{"burglar", burglar},
		{"lock-metrics-notifier", lockMetricsNotifier},
		{"db-metrics-notifier", dbMetricsNotifier},
		{"request-metrics-notifier", requestNotifier},
	}

	if cfg.EnableConsulServiceRegistration {
		consulClient, err := consuladapter.NewClientFromUrl(cfg.ConsulCluster)
		if err != nil {
			logger.Fatal("new-consul-client-failed", err)
		}
		registrationRunner := initializeRegistrationRunner(logger, consulClient, portNum, clock)
		members = append(members, grouper.Member{"registration-runner", registrationRunner})
	}

	if cfg.DebugAddress != "" {
		members = append(grouper.Members{
			{"debug-server", debugserver.Runner(cfg.DebugAddress, reconfigurableSink)},
		}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)
	monitor := ifrit.Invoke(sigmon.New(group))

	logger.Info("started")

	go func() {
		<-exitCh
		logger.Info("shutting-down-due-to-unrecoverable-error")
		monitor.Signal(os.Interrupt)
	}()

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}
}

func initializeMetron(logger lager.Logger, locketConfig config.LocketConfig) (loggingclient.IngressClient, error) {
	client, err := loggingclient.NewIngressClient(locketConfig.LoggregatorConfig)
	if err != nil {
		return nil, err
	}

	if locketConfig.LoggregatorConfig.UseV2API {
		emitter := runtimeemitter.NewV1(client)
		go emitter.Run()
	}

	return client, nil
}

func appendExtraConnectionStringParam(logger lager.Logger, driverName, databaseConnectionString, sqlCACertFile string) string {
	switch driverName {
	case "mysql":
		cfg, err := mysql.ParseDSN(databaseConnectionString)
		if err != nil {
			logger.Fatal("invalid-db-connection-string", err, lager.Data{"connection-string": databaseConnectionString})
		}

		if sqlCACertFile != "" {
			certBytes, err := ioutil.ReadFile(sqlCACertFile)
			if err != nil {
				logger.Fatal("failed-to-read-sql-ca-file", err)
			}

			caCertPool := x509.NewCertPool()
			if ok := caCertPool.AppendCertsFromPEM(certBytes); !ok {
				logger.Fatal("failed-to-parse-sql-ca", err)
			}

			tlsConfig := &tls.Config{
				InsecureSkipVerify: false,
				RootCAs:            caCertPool,
			}

			mysql.RegisterTLSConfig("bbs-tls", tlsConfig)
			cfg.TLSConfig = "bbs-tls"
		}
		cfg.Timeout = 10 * time.Minute
		cfg.ReadTimeout = 10 * time.Minute
		cfg.WriteTimeout = 10 * time.Minute
		databaseConnectionString = cfg.FormatDSN()
	case "postgres":
		var err error
		databaseConnectionString, err = pq.ParseURL(databaseConnectionString)
		if err != nil {
			logger.Fatal("invalid-db-connection-string", err, lager.Data{"connection-string": databaseConnectionString})
		}
		if sqlCACertFile == "" {
			databaseConnectionString = databaseConnectionString + " sslmode=disable"
		} else {
			databaseConnectionString = fmt.Sprintf("%s sslmode=verify-ca sslrootcert=%s", databaseConnectionString, sqlCACertFile)
		}
	}

	return databaseConnectionString
}

func initializeRegistrationRunner(
	logger lager.Logger,
	consulClient consuladapter.Client,
	port int,
	clock clock.Clock,
) ifrit.Runner {
	registration := &api.AgentServiceRegistration{
		Name: "locket",
		Port: port,
		Check: &api.AgentServiceCheck{
			TTL: "20s",
		},
	}
	return locket.NewRegistrationRunner(logger, registration, consulClient, locket.RetryInterval, clock)
}
