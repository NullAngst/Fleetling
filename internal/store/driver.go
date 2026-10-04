package store

// The SQLite driver lives in its own file so the rest of the package only
// sees database/sql. modernc.org/sqlite is pure Go, which keeps the binary
// static with CGO_ENABLED=0.
import _ "modernc.org/sqlite"

// driverName is the name modernc.org/sqlite registers with database/sql.
const driverName = "sqlite"
