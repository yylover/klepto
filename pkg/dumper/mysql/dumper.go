package mysql

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"fmt"
	"github.com/go-sql-driver/mysql"
	log "github.com/sirupsen/logrus"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/hellofresh/klepto/pkg/database"
	"github.com/hellofresh/klepto/pkg/dumper"
	"github.com/hellofresh/klepto/pkg/dumper/engine"
	"github.com/hellofresh/klepto/pkg/reader"
)

const (
	null = "NULL"
)

type (
	myDumper struct {
		conn                *sql.DB
		reader              reader.Reader
		setGlobalInline     sync.Once
		disableGlobalInline bool
		batch               int64
	}
)

// NewDumper returns a new mysql dumper.
func NewDumper(conn *sql.DB, rdr reader.Reader, batch int64) dumper.Dumper {
	return engine.New(rdr, &myDumper{
		conn:   conn,
		reader: rdr,
		batch:  batch,
	})
}

// DumpStructure dump the mysql database structure.
func (d *myDumper) DumpStructure(sql string) error {
	if _, err := d.conn.Exec(sql); err != nil {
		return err
	}

	return nil
}

// DumpTable dumps a mysql table.
func (d *myDumper) DumpTable(tableName string, rowChan <-chan database.Row) error {
	var err error
	d.setGlobalInline.Do(func() {
		var allowLocalInline bool
		r := d.conn.QueryRow("SELECT @@GLOBAL.local_infile")
		if err = r.Scan(&allowLocalInline); err != nil {
			return
		}

		if allowLocalInline {
			return
		}

		if _, err = d.conn.Exec("SET GLOBAL local_infile=1"); err != nil {
			return
		}
		d.disableGlobalInline = true
	})
	if err != nil {
		return err
	}

	batch := d.batch
	inserted := batch
	for inserted == batch {
		inserted, err = d.trunkInsert(tableName, rowChan, batch)
		if err != nil {
			log.Panic(err)
		}
	}

	return nil
}

func (d *myDumper) trunkInsert(tableName string, rowChan <-chan database.Row, batch int64) (int64, error) {
	txn, err := d.conn.Begin()
	if err != nil {
		return 0, fmt.Errorf("failed to open transaction: %w", err)
	}

	insertedRows, err := d.insertIntoTable(txn, tableName, rowChan, batch)
	if err != nil {
		defer func() {
			if err := txn.Rollback(); err != nil {
				log.WithError(err).Error("failed to rollback")
			}
		}()
		err = fmt.Errorf("failed to insert rows: %w", err)
		return 0, err
	}

	log.WithFields(log.Fields{
		"table":    tableName,
		"inserted": insertedRows,
	}).Debug("inserted rows")

	if err := txn.Commit(); err != nil {
		return 0, fmt.Errorf("failed to commit transaction: %w", err)
	}

	return insertedRows, nil
}

// Close closes the mysql database connection.
func (d *myDumper) Close() error {
	var errGlobalInline error
	if d.disableGlobalInline {
		_, errGlobalInline = d.conn.Exec("SET GLOBAL local_infile=0")
	}

	err := d.conn.Close()
	if err != nil {
		if errGlobalInline != nil {
			return fmt.Errorf("failed to close mysql connection and `SET GLOBAL local_infile=0`: %w", errGlobalInline)
		}

		return fmt.Errorf("failed to close mysql connection: %w", err)
	}

	if errGlobalInline != nil {
		return fmt.Errorf("failed `SET GLOBAL local_infile=0` please do this manually! : %w", errGlobalInline)
	}

	return nil
}

func (d *myDumper) insertIntoTable(txn *sql.Tx, tableName string, rowChan <-chan database.Row, batch int64) (int64, error) {
	columns, err := d.reader.GetColumns(tableName)
	if err != nil {
		return 0, fmt.Errorf("failed to get columns: %w", err)
	}

	columnsQuoted := make([]string, len(columns))
	for i, column := range columns {
		columnsQuoted[i] = d.quoteIdentifier(column)
	}

	query := fmt.Sprintf(
		"LOAD DATA CONCURRENT LOCAL INFILE 'Reader::%s' INTO TABLE %s FIELDS TERMINATED BY ',' ENCLOSED BY '\"' ESCAPED BY '\"' (%s)",
		tableName,
		d.quoteIdentifier(tableName),
		strings.Join(columnsQuoted, ","),
	)

	fmt.Println("LOAD DATA : ", query)

	// Write all rows as csv to the pipe
	rowReader, rowWriter := io.Pipe()
	var inserted int64
	go func(writer *io.PipeWriter) {
		defer writer.Close()

		w := csv.NewWriter(writer)
		defer w.Flush()

		for {
			row, more := <-rowChan
			if !more {
				break
			}

			// Put the data in the correct order and format
			rowValues := make([]string, len(columns))
			for i, col := range columns {
				switch v := row[col].(type) {
				case nil:
					rowValues[i] = null
				case string:
					rowValues[i] = row[col].(string)
				case []uint8:
					rowValues[i] = string(row[col].([]uint8))
				default:
					log.WithField("type", v).Info("we have an unhandled type. attempting to convert to a string \n")
					rowValues[i] = row[col].(string)
				}
			}

			if err := w.Write(rowValues); err != nil {
				log.WithError(err).Error("error writing record to mysql")
			}

			atomic.AddInt64(&inserted, 1)
			if inserted >= batch {
				break
			}
		}
	}(rowWriter)

	// Register the reader for reading the csv
	mysql.RegisterReaderHandler(tableName, func() io.Reader { return rowReader })
	defer mysql.DeregisterReaderHandler(tableName)

	if _, err := txn.Exec("SET foreign_key_checks = 0;"); err != nil {
		return 0, fmt.Errorf("failed to disable foreign key checks: %w", err)
	}

	if _, err := txn.Exec(query); err != nil {
		return 0, fmt.Errorf("failed to execute query: %w", err)
	}

	return inserted, nil
}

func (d *myDumper) insertIntoTableReplace(txn *sql.Tx, tableName string, rowChan <-chan database.Row, batch int64) (int64, error) {
	columns, err := d.reader.GetColumns(tableName)
	if err != nil {
		return 0, fmt.Errorf("failed to get columns: %w", err)
	}

	columnsQuoted := make([]string, len(columns))
	for i, column := range columns {
		columnsQuoted[i] = d.quoteIdentifier(column)
	}

	buf := bytes.Buffer{}
	BufSizeLimit := 1 * 1024 * 1024 // 1MB. TODO parameterize it
	BufSizeLimitDelta := 1024
	buf.Grow(BufSizeLimit + BufSizeLimitDelta)

	buf.WriteString(fmt.Sprintf(`replace into %s values `,
		tableName))

	var inserted int64
	for {
		row, more := <-rowChan
		if !more {
			break
		}

		// Put the data in the correct order and format
		rowValues := make([]string, len(columns))
		for i, col := range columns {
			switch v := row[col].(type) {
			case nil:
				rowValues[i] = null
			case string:
				rowValues[i] = row[col].(string)
			case []uint8:
				rowValues[i] = string(row[col].([]uint8))
			default:
				log.WithField("type", v).Info("we have an unhandled type. attempting to convert to a string \n")
				rowValues[i] = row[col].(string)
			}
		}
		if inserted != 0 {
			buf.WriteString(",")
		}
		buf.WriteString("(" + strings.Join(rowValues, ",") + ")")

		atomic.AddInt64(&inserted, 1)
		if inserted >= batch {
			break
		}
	}

	if inserted == 0 {
		return 0, nil
	}

	//fmt.Println("query", buf.String())
	if _, err := txn.Exec("SET foreign_key_checks = 0;"); err != nil {
		return 0, fmt.Errorf("failed to disable foreign key checks: %w", err)
	}

	if _, err := txn.Exec(buf.String()); err != nil {
		return 0, fmt.Errorf("failed to execute query: %w", err)
	}

	return inserted, nil
}

func (d *myDumper) quoteIdentifier(name string) string {
	return fmt.Sprintf("`%s`", strings.Replace(name, "`", "``", -1))
}
