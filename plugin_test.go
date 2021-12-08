// Copyright The OpenTelemetry Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package otelgorm

import (
	"context"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type TestModel struct {
	ID    uint `gorm:"primarykey"`
	Code  string
	Price uint
}

func initDB() (*gorm.DB, error) {
	var (
		err    error
		dbFile *os.File
		db     *gorm.DB
	)

	dbFile, err = ioutil.TempFile("", "db")
	defer func() {
		if err == nil {
			return
		}

		if dbFile != nil {
			os.Remove(dbFile.Name())
		}

		if db != nil {
			closeDB(db)
		}
	}()

	if err != nil {
		return nil, err
	}

	// Initialize db connection
	db, err = gorm.Open(sqlite.Open(dbFile.Name()), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// Migrate the schema
	err = db.AutoMigrate(&TestModel{})
	if err != nil {
		return nil, err
	}

	return db, nil
}

func closeDB(db *gorm.DB) {
	sqlDB, err := db.DB()
	if err != nil {
		sqlDB.Close()
	}
}

// nolint:funlen,gocognit // breaking testCase will break the readability
func TestPlugin(t *testing.T) {
	testCases := []struct {
		desc   string
		testOp func(db *gorm.DB) *gorm.DB
		spans        int
		targetSpan   int
		sqlOp        string
		affectedRows int64
	}{
		{
			"create (insert) row",
			func(db *gorm.DB) *gorm.DB { return db.Create(&TestModel{Code: "D42", Price: 100}) },
			2, 0, "INSERT", 1,
		},
		{
			"save (update) row",
			func(db *gorm.DB) *gorm.DB {
				tm := TestModel{ID: 1, Code: "D42", Price: 100}
				db = db.Create(&tm)
				if db.Error != nil {
					return db
				}
				tm.Code = "foo"
				return db.Save(&tm)
			},
			3, 1, "UPDATE", 1,
		},
		{
			"delete row",
			func(db *gorm.DB) *gorm.DB {
				tm := TestModel{ID: 1, Code: "D42", Price: 100}
				db = db.Create(&tm)
				if db.Error != nil {
					return db
				}
				return db.Delete(&tm)
			},
			3, 1, "DELETE", 1,
		},
		{
			"query row",
			func(db *gorm.DB) *gorm.DB {
				tm := TestModel{ID: 1, Code: "D42", Price: 100}
				db = db.Create(&tm)
				if db.Error != nil {
					return db
				}
				return db.First(&tm)
			},
			3, 1, "SELECT", 1,
		},
		{
			"raw",
			func(db *gorm.DB) *gorm.DB {
				tm := TestModel{ID: 1, Code: "D42", Price: 100}
				db = db.Create(&tm)
				if db.Error != nil {
					return db
				}

				var result []TestModel
				return db.Raw("SELECT * FROM test_models").Scan(&result)
			},
			3, 1, "SELECT", -1,
		},
		{
			"row",
			func(db *gorm.DB) *gorm.DB {
				tm := TestModel{ID: 1, Code: "D42", Price: 100}
				db = db.Create(&tm)
				if db.Error != nil {
					return db
				}

				db.Raw("SELECT id FROM test_models").Row()
				return &gorm.DB{Error: nil}
			},
			3, 1, "SELECT", -1,
		},
	}

	for i, test := range testCases {
			db, err := initDB()
			defer closeDB(db)

			assert.NoError(t, err)

			sr := tracetest.NewSpanRecorder()
			provider := trace.NewTracerProvider(trace.WithSpanProcessor(sr))
			plugin := NewPlugin(WithTracerProvider(provider))

			err = db.Use(plugin)
			assert.NoError(t, err)

			ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
			defer cancel()

			ctx, span := provider.Tracer(defaultTracerName).Start(ctx, "gorm-test")

			db = db.WithContext(ctx)
			// Create
			dbOp := test.testOp(db)
			assert.NoError(t, dbOp.Error)

			span.End()

			spans := sr.Ended()
			require.Len(t, spans, test.spans)
			s := spans[test.targetSpan]

			attributes := s.Attributes()

			assert.Equal(t, spanName, s.Name(), "TEST[%v] %v",i,test.desc)
			assert.Equal(t, spans[0].SpanContext().TraceID().String(), spans[1].SpanContext().TraceID().String(),attributes,"TEST[%v] %v",i,test.desc)
			assert.Equal(t, "test_models", attributes[0].Value.AsString(),"TEST[%v] %v",i,test.desc)
			assert.Contains(t, attributes[1].Value.AsString(), test.sqlOp,"TEST[%v] %v",i,test.desc)
			assert.Equal(t, test.sqlOp, attributes[2].Value.AsString(),"TEST[%v] %v",i,test.desc)
			assert.Equal(t, test.affectedRows, attributes[3].Value.AsInt64(),"TEST[%v] %v",i,test.desc)
	}
}
