package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	elastic "gopkg.in/olivere/elastic.v5"

	logit "github.com/brettallred/go-logit"
	"github.com/stretchr/testify/assert"
)

func init() {
	os.Setenv("PLATFORM_ENV", "test")
}

// TestFlow is basically an end to end integration test
func TestFlow(t *testing.T) {
	initialSetup()

	conn := pool.Get()
	defer conn.Close()

	sample := readPayload("execute.json")
	conn.Do("LPUSH", redisKey(), sample)

	llen, err := conn.Do("LLEN", redisKey())
	assert.NoError(t, err)
	assert.Equal(t, int64(1), llen)

	message := "duration: 0.051 ms  execute <unnamed>: select * from servers where id IN ('1', '2', '3') and name = 'localhost'"
	query, err := getLog()
	assert.NoError(t, err)
	assert.Equal(t, message, query.message)

	assert.Equal(t, 0.051, query.totalDuration)
	assert.Equal(t, "execute", query.preparedStep)
	assert.Equal(t, "<unnamed>", query.prepared)
	assert.Equal(t, "select * from servers where id IN ('1', '2', '3') and name = 'localhost'", query.query)

	pgQuery := "select * from servers where id IN (?) and name = ?"
	assert.Equal(t, pgQuery, query.uniqueStr)

	assert.Equal(t, 0, len(batchMap))
	_, ok := batchMap[batch{mockCurrentMinute(), query.uniqueSha}]
	assert.False(t, ok)
	addToQueries(mockCurrentMinute(), query)
	assert.Equal(t, 1, len(batchMap))
	assert.Equal(t, int32(1), batchMap[batch{mockCurrentMinute(), query.uniqueSha}].totalCount)

	addToQueries(mockCurrentMinute(), query)
	_, ok = batchMap[batch{mockCurrentMinute(), query.uniqueSha}]
	assert.True(t, ok)
	assert.Equal(t, 1, len(batchMap))
	assert.Equal(t, int32(2), batchMap[batch{mockCurrentMinute(), query.uniqueSha}].totalCount)

	iterOverQueries()
	assert.Equal(t, 0, len(batchMap))

	err = bulkProc["bulk"].Flush()
	if err != nil {
		logit.Error("Error flushing messages: %e", err.Error())
	}
	totalDuration := getRecord()
	assert.Equal(t, 0.102, totalDuration)

	conn.Do("DEL", redisKey())
	defer bulkProc["bulk"].Close()
	defer clients["bulk"].Stop()
}

func TestTempTable(t *testing.T) {
	initialSetup()

	conn := pool.Get()
	defer conn.Close()

	sample := readPayload("temp_table.json")
	conn.Do("LPUSH", redisKey(), sample)

	message := "temporary file: path \"base/pgsql_tmp/pgsql_tmp73093.7\", size 2576060"
	grokQuery := "SELECT DISTINCT \"users\".* FROM \"users\" LEFT JOIN location_users ON location_users.employee_id = users.id WHERE \"users\".\"active\" = 't' AND (location_users.location_id = 17511 OR (users.organization_id = 7528 AND users.role = 'Client'))"
	query, err := getLog()
	assert.NoError(t, err)
	assert.Equal(t, message, query.message)
	assert.Equal(t, grokQuery, query.query)
	assert.Equal(t, int64(2576060), query.tempTable)

	assert.Equal(t, 0, len(batchMap))
	_, ok := batchMap[batch{mockCurrentMinute(), query.uniqueSha}]
	assert.False(t, ok)
	addToQueries(mockCurrentMinute(), query)
	assert.Equal(t, 1, len(batchMap))
	assert.Equal(t, int32(1), batchMap[batch{mockCurrentMinute(), query.uniqueSha}].totalCount)

	iterOverQueries()
	assert.Equal(t, 0, len(batchMap))

	err = bulkProc["bulk"].Flush()
	if err != nil {
		logit.Error("Error flushing messages: %e", err.Error())
	}
	tmpTable := getRecordWithTempTable()
	assert.Equal(t, int64(2576060), tmpTable)

	conn.Do("DEL", redisKey())
	defer bulkProc["bulk"].Close()
	defer clients["bulk"].Stop()
}

func TestUpdateWaiting(t *testing.T) {
	initialSetup()

	conn := pool.Get()
	defer conn.Close()

	sample := readPayload("update_waiting.json")
	conn.Do("LPUSH", redisKey(), sample)

	llen, err := conn.Do("LLEN", redisKey())
	assert.NoError(t, err)
	assert.Equal(t, int64(1), llen)

	notes := "process 11451 acquired ExclusiveLock on page 0 of relation 519373 of database 267504 after 1634.121 ms"
	query, err := getLog()
	assert.NoError(t, err)
	assert.Equal(t, notes, query.notes)

	message := "UPDATE \"review_invitations\" SET \"mms_url\" = $1, \"sms_text\" = $2, \"message_sid\" = $3, \"updated_at\" = $4 WHERE \"review_invitations\".\"id\" = $5"
	assert.Equal(t, message, query.uniqueStr)
}

func readPayload(filename string) []byte {
	dat, err := ioutil.ReadFile("./sample_payloads/" + filename)
	check(err)
	return dat
}

// TestCurrentMinute basically tests currentMinute()
func TestCurrentMinute(t *testing.T) {
	d := time.Date(2017, time.November, 10, 23, 19, 5, 1250, time.UTC)
	minute := d.UTC().Round(time.Minute)
	assert.Equal(t, 0, minute.Second())
}

func TestRound(t *testing.T) {
	r := round(0.564627465465, 0.5, 5)
	assert.Equal(t, 0.56463, r)
}

func mockCurrentMinute() time.Time {
	d := time.Date(2017, time.October, 27, 19, 57, 5, 1250, time.UTC)
	return d.UTC().Round(time.Minute)
}

func getRecord() float64 {
	termQuery := elastic.NewTermQuery("user_name", "samplepayload")
	result, err := clients["bulk"].Search().
		Index(indexName()).
		Type("pglog").
		Query(termQuery).
		From(0).Size(1).
		Do(context.Background())
	if err != nil {
		panic(err)
	}

	if result.Hits.TotalHits > 0 {
		fmt.Printf("Found a total of %d record(s)\n", result.Hits.TotalHits)

		for _, hit := range result.Hits.Hits {
			// hit.Index contains the name of the index

			var data map[string]*json.RawMessage
			if err := json.Unmarshal(*hit.Source, &data); err != nil {
				logit.Error("Error unmarshalling data: %e", err.Error())
			}

			var totalDuration float64
			if source, pres := data["total_duration_ms"]; pres {
				if err := json.Unmarshal(*source, &totalDuration); err != nil {
					logit.Error("Error unmarshalling totalDuration: %e", err.Error())
				}
			}

			fmt.Printf("First record found has a total duration of %f\n", totalDuration)
			return totalDuration
		}
	} else {
		// No hits
		fmt.Print("Found no records, waiting 500ms...\n")
		time.Sleep(500 * time.Millisecond)
		return getRecord()
	}
	return -1.0
}

func getRecordWithTempTable() int64 {
	fmt.Println("getRecordWithTempTable")

	termQuery := elastic.NewTermQuery("user_name", "temp_table")
	result, err := clients["bulk"].Search().
		Index(indexName()).
		Type("pglog").
		Query(termQuery).
		From(0).Size(1).
		Do(context.Background())
	if err != nil {
		panic(err)
	}

	if result.Hits.TotalHits > 0 {
		fmt.Printf("Found a total of %d record(s)\n", result.Hits.TotalHits)

		for _, hit := range result.Hits.Hits {
			// hit.Index contains the name of the index

			var data map[string]*json.RawMessage
			if err := json.Unmarshal(*hit.Source, &data); err != nil {
				logit.Error("Error unmarshalling data: %e", err.Error())
			}

			var tempTable int64
			if source, pres := data["temp_table_size"]; pres {
				if err := json.Unmarshal(*source, &tempTable); err != nil {
					logit.Error("Error unmarshalling tempTable: %e", err.Error())
				}
			}

			fmt.Printf("First record found has a total temp table size of %d\n", tempTable)
			return tempTable
		}
	} else {
		// No hits
		fmt.Print("Found no records, waiting 500ms...\n")
		time.Sleep(500 * time.Millisecond)
		return getRecordWithTempTable()
	}
	return -1
}
