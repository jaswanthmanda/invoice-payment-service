package models

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const dateLayout = "2006-01-02"

// Date is a calendar day. It reads and writes JSON as "YYYY-MM-DD" and maps
// to a Postgres DATE column.
type Date struct{ time.Time }

func ParseDate(s string) (Date, error) {
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return Date{}, err
	}
	return Date{t}, nil
}

func (d Date) String() string { return d.Format(dateLayout) }

func (d Date) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return errors.New("due_date must be a string in YYYY-MM-DD format")
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return errors.New("due_date must be a valid date in YYYY-MM-DD format")
	}
	*d = parsed
	return nil
}

// Value implements driver.Valuer (writing to the database).
func (d Date) Value() (driver.Value, error) { return d.Time, nil }

// Scan implements sql.Scanner (reading from the database).
func (d *Date) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		d.Time = time.Time{}
		return nil
	case time.Time:
		d.Time = v
		return nil
	case string:
		return d.parseText(v)
	case []byte:
		return d.parseText(string(v))
	default:
		return fmt.Errorf("cannot scan %T into Date", src)
	}
}

func (d *Date) parseText(s string) error {
	if len(s) >= 10 {
		s = s[:10]
	}
	t, err := time.Parse(dateLayout, s)
	if err != nil {
		return err
	}
	d.Time = t
	return nil
}
