package mysql

import (
	"database/sql"
	"fmt"

	"github.com/go-sql-driver/mysql"
	log "github.com/sirupsen/logrus"

	"github.com/hellofresh/klepto/pkg/dumper"
	"github.com/hellofresh/klepto/pkg/reader"
)

type driver struct{}

// IsSupported checks if the given dsn connection string is supported.
func (m *driver) IsSupported(dsn string) bool {
	if dsn == "" {
		return false
	}

	_, err := mysql.ParseDSN(dsn)
	return err == nil
}

// NewConnection creates a new mysql connection and retrieves a new mysql dumper.
func (m *driver) NewConnection(opts dumper.ConnOpts, rdr reader.Reader) (dumper.Dumper, error) {
	dsnCfg, err := mysql.ParseDSN(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("failed to parse mysql dsn: %w", err)
	}

	if !dsnCfg.MultiStatements {
		log.WithField("help", "https://github.com/go-sql-driver/mysql#multistatements").
			Warning("MYSQL dumper forcing multistatements!")
		dsnCfg.MultiStatements = true
	}

	conn, err := sql.Open("mysql", dsnCfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("failed to open mysql connection: %w", err)
	}

	conn.SetMaxOpenConns(opts.MaxConns)
	conn.SetMaxIdleConns(opts.MaxIdleConns)
	conn.SetConnMaxLifetime(opts.MaxConnLifetime)

	return NewDumper(conn, rdr, opts.Batch), nil
}

func init() {
	dumper.Register("mysql", &driver{})
}
