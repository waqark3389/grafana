package postgres

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util/errutil"

	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/tsdb"
	"github.com/grafana/grafana/pkg/tsdb/sqleng"
	"xorm.io/core"
)

func init() {
	tsdb.RegisterTsdbQueryEndpoint("postgres", newPostgresQueryEndpoint)
}

func newPostgresQueryEndpoint(datasource *models.DataSource) (tsdb.TsdbQueryEndpoint, error) {
	logger := log.New("tsdb.postgres")
	logger.Debug("Creating Postgres query endpoint")

	cnnstr, err := generateConnectionString(datasource, logger)
	if err != nil {
		return nil, err
	}

	if setting.Env == setting.DEV {
		logger.Debug("getEngine", "connection", cnnstr)
	}

	config := sqleng.SqlQueryEndpointConfiguration{
		DriverName:        "postgres",
		ConnectionString:  cnnstr,
		Datasource:        datasource,
		MetricColumnTypes: []string{"UNKNOWN", "TEXT", "VARCHAR", "CHAR"},
	}

	queryResultTransformer := postgresQueryResultTransformer{
		log: logger,
	}

	timescaledb := datasource.JsonData.Get("timescaledb").MustBool(false)

	endpoint, err := sqleng.NewSqlQueryEndpoint(&config, &queryResultTransformer, newPostgresMacroEngine(timescaledb), logger)
	if err != nil {
		logger.Debug("Failed connecting to Postgres", "err", err)
		return nil, err
	}

	logger.Debug("Successfully connected to Postgres")
	return endpoint, err
}

func generateConnectionString(datasource *models.DataSource, logger log.Logger) (string, error) {
	logger.Debug("Trying to generate Postgres connection string")

	sslMode := strings.TrimSpace(strings.ToLower(datasource.JsonData.Get("sslmode").MustString("verify-full")))
	isSSLDisabled := sslMode == "disable"

	reHost := regexp.MustCompile(`^([^/].*(?::\d+)?)|(/.*)$`)
	ms := reHost.FindStringSubmatch(datasource.Url)
	if len(ms) == 0 {
		return "", fmt.Errorf("invalid host specifier: %q", datasource.Url)
	}

	var host string
	var port int
	if ms[1] != "" {
		sp := strings.SplitN(ms[1], ":", 2)
		host = sp[0]
		var err error
		port, err = strconv.Atoi(sp[1])
		if err != nil {
			return "", errutil.Wrapf(err, "invalid port in host specifier %q", ms[1])
		}

		logger.Debug("Generating connection string with network host/port pair", "host", host, "port", port)
	} else {
		host = ms[2]
		logger.Debug("Generating connection string with Unix socket specifier", "socket", host)
	}

	connStr := fmt.Sprintf("user='%s' password='%s' host='%s' dbname='%s' sslmode='%s'",
		datasource.User, datasource.DecryptedPassword(), host, datasource.Database, sslMode)
	if port > 0 {
		connStr += fmt.Sprintf(" port=%d", port)
	}
	if isSSLDisabled {
		logger.Debug("Postgres SSL is disabled")
	} else {
		logger.Debug("Postgres SSL is enabled", "sslMode", sslMode)

		// Attach root certificate if provided
		if sslRootCert := datasource.JsonData.Get("sslRootCertFile").MustString(""); sslRootCert != "" {
			logger.Debug("Setting server root certificate", "sslRootCert", sslRootCert)
			connStr += fmt.Sprintf(" sslrootcert='%s'", sslRootCert)
		}

		// Attach client certificate and key if both are provided
		sslCert := datasource.JsonData.Get("sslCertFile").MustString("")
		sslKey := datasource.JsonData.Get("sslKeyFile").MustString("")
		if sslCert != "" && sslKey != "" {
			logger.Debug("Setting SSL client auth", "sslCert", sslCert, "sslKey", sslKey)
			connStr += fmt.Sprintf(" sslcert='%s' sslkey='%s'", sslCert, sslKey)
		} else if sslCert != "" || sslKey != "" {
			return "", fmt.Errorf("SSL client certificate and key must both be specified")
		}
	}

	logger.Debug("Generated Postgres connection string successfully")
	return connStr, nil
}

type postgresQueryResultTransformer struct {
	log log.Logger
}

func (t *postgresQueryResultTransformer) TransformQueryResult(columnTypes []*sql.ColumnType, rows *core.Rows) (tsdb.RowValues, error) {
	values := make([]interface{}, len(columnTypes))
	valuePtrs := make([]interface{}, len(columnTypes))

	for i := 0; i < len(columnTypes); i++ {
		valuePtrs[i] = &values[i]
	}

	if err := rows.Scan(valuePtrs...); err != nil {
		return nil, err
	}

	// convert types not handled by lib/pq
	// unhandled types are returned as []byte
	for i := 0; i < len(columnTypes); i++ {
		if value, ok := values[i].([]byte); ok {
			switch columnTypes[i].DatabaseTypeName() {
			case "NUMERIC":
				if v, err := strconv.ParseFloat(string(value), 64); err == nil {
					values[i] = v
				} else {
					t.log.Debug("Rows", "Error converting numeric to float", value)
				}
			case "UNKNOWN", "CIDR", "INET", "MACADDR":
				// char literals have type UNKNOWN
				values[i] = string(value)
			default:
				t.log.Debug("Rows", "Unknown database type", columnTypes[i].DatabaseTypeName(), "value", value)
				values[i] = string(value)
			}
		}
	}

	return values, nil
}

func (t *postgresQueryResultTransformer) TransformQueryError(err error) error {
	return err
}
