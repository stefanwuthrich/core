// Package mysql implements MySQL migrations.
package mysql

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/stefanwuthrich/core/file"
	"github.com/stefanwuthrich/core/storage"
	database "github.com/stefanwuthrich/core/storage/driver/mysql"
	"github.com/stefanwuthrich/core/storage/migration"
	"github.com/jmoiron/sqlx"
)

// *****************************************************************************
// Thread-Safe Configuration
// *****************************************************************************

var (
	info      database.Info
	infoMutex sync.RWMutex
)

// SetConfig stores the config.
func SetConfig(i database.Info) {
	infoMutex.Lock()
	info = i
	infoMutex.Unlock()
}

// ResetConfig removes the config.
func ResetConfig() {
	infoMutex.Lock()
	info = database.Info{}
	infoMutex.Unlock()
}

// Config returns the config.
func Config() database.Info {
	infoMutex.RLock()
	defer infoMutex.RUnlock()
	return info
}

// Configuration defines the shared configuration interface.
type Configuration struct {
	database.Info
}

// Shared returns the global configuration information.
func Shared() Configuration {
	return Configuration{
		Config(),
	}
}

// *****************************************************************************
// Migration Creation
// *****************************************************************************

var (
	migrationTable = "migration"
)

// New creates a migration connection to the database.
func (c Configuration) New() (*migration.Info, error) {
	var mig *migration.Info

	// Load the config
	i := c.Info

	// Build the path to the mysql migration folder
	projectRoot := filepath.Dir(os.Getenv("JAYCONFIG"))
	folder := filepath.Join(projectRoot, i.MigrationFolder)

	// If the folder doesn't exist
	if !file.Exists(folder) {
		// Set to the current folder
		dir, _ := os.Getwd()
		folder = filepath.Join(dir, i.MigrationFolder)
	}

	// Create MySQL entity
	mi := &Entity{}

	// Update the config
	mi.UpdateConfig(&i)

	// Connect to the database
	con, err := i.Connect(true)

	// If the database doesn't exist or can't connect
	if err != nil {
		// Close the open connection (since 'unknown database' is still an
		// active connection)
		con.Close()

		// Connect to database without a database
		con, err = i.Connect(false)
		if err != nil {
			return mig, err
		}

		// Create the database
		err = i.Create(con)
		if err != nil {
			return mig, err
		}

		// Close connection
		con.Close()

		// Reconnect to the database
		con, err = i.Connect(true)
		if err != nil {
			return mig, err
		}
	}

	// Store the connection in the entity
	mi.sql = con

	// Setup logic was here
	return migration.New(mi, folder)
}

// *****************************************************************************
// Interface
// *****************************************************************************

// Extension returns the file extension with a period
func (t *Entity) Extension() string {
	return ".sql"
}

// UpdateConfig will update any parameters necessary
func (t *Entity) UpdateConfig(config *database.Info) {
	config.Parameter = "parseTime=true&multiStatements=true"
}

// TableExist returns true if the migration table exists
func (t *Entity) TableExist() error {
	_, err := t.sql.Exec(fmt.Sprintf("SELECT 1 FROM %v LIMIT 1;", migrationTable))
	if err != nil {
		return err
	}

	return err
}

// CreateTable returns true if the migration was created
func (t *Entity) CreateTable() error {
	_, err := t.sql.Exec(fmt.Sprintf(`CREATE TABLE %v (
		id INT UNSIGNED NOT NULL AUTO_INCREMENT,
  		name VARCHAR(191) NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		UNIQUE KEY (name),
  		PRIMARY KEY (id)
		);`, migrationTable))

	if err != nil {
		return err
	}

	return err
}

// Status returns last migration name
func (t *Entity) Status() (string, error) {
	result := &Entity{}
	err := t.sql.Get(result, fmt.Sprintf("SELECT * FROM %v ORDER BY id DESC LIMIT 1;", migrationTable))

	// If no rows, then set to nil
	if err == sql.ErrNoRows {
		err = nil
	}

	return result.Name, err
}

// statusID returns last migration ID
func (t *Entity) statusID() (uint32, error) {
	result := &Entity{}
	err := t.sql.Get(result, fmt.Sprintf("SELECT * FROM %v ORDER BY id DESC LIMIT 1;", migrationTable))
	return result.ID, err
}

// Migrate runs a query and returns error
func (t *Entity) Migrate(qry string) error {
	_, err := t.sql.Exec(qry)
	return err
}

// RecordUp adds a record to the database
func (t *Entity) RecordUp(name string) error {
	_, err := t.sql.Exec(fmt.Sprintf("INSERT INTO %v (name) VALUES (?);", migrationTable), name)
	return err
}

// RecordDown removes a record from the database and updates the AUTO_INCREMENT value
func (t *Entity) RecordDown(name string) error {
	_, err := t.sql.Exec(fmt.Sprintf("DELETE FROM %v WHERE name = ? LIMIT 1;", migrationTable), name)

	// If the record was removed successfully
	if err == nil {
		var ID uint32
		var nextID uint32 = 1

		// Get the last migration record now
		ID, err = t.statusID()

		// If there are no more migrations in the table
		if err == sql.ErrNoRows {
			// Leave ID at 1
		} else if err != nil {
			return err
		} else {
			nextID = ID
		}

		_, err = t.sql.Exec(fmt.Sprintf("ALTER TABLE %v AUTO_INCREMENT = %v;", migrationTable, nextID))
	}
	return err
}

// Entity defines the migration table
type Entity struct {
	ID        uint32    `db:"id"`
	Name      string    `db:"name"`
	CreatedAt time.Time `db:"created_at"`
	sql       *sqlx.DB
}

// *****************************************************************************
// Test Helpers
// *****************************************************************************

// SetUp is a function for unit tests on a separate database.
func SetUp(envPath string, dbName string) (*migration.Info, Configuration) {
	// Get the environment variable
	if len(os.Getenv("JAYCONFIG")) == 0 {
		// Attempt to find env.json
		p, err := filepath.Abs(envPath)
		if err != nil {
			log.Fatalf("%v", err)
		}

		// Set the environment variable
		os.Setenv("JAYCONFIG", p)
	}

	// Load the config
	config, err := storage.LoadConfig(os.Getenv("JAYCONFIG"))
	if err != nil {
		log.Fatalf("%v", err)
	}
	config.MySQL.Database = dbName

	// Create the migration configuration
	conf := Configuration{
		config.MySQL,
	}

	// Create the migration
	mig, err := conf.New()
	if err != nil {
		log.Fatalf("%v", err)
	}

	// Refresh the data
	mig.DownAll()
	mig.UpAll()

	return mig, conf
}

// TearDown removes the unit test database.
func TearDown(db *sqlx.DB, dbName string) error {
	// Drop the database
	_, err := db.Exec(fmt.Sprintf(`DROP DATABASE %v;`, dbName))
	return err
}
