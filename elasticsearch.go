package main

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/mattbaird/elastigo/lib"
)

const (
	rawMsgKey    = "@raw_msg"
	timestampKey = "@timestamp"
	sourceKey    = "@source"
)

type payload map[string]interface{}

func newPayload(msg, source string) *payload {
	return &payload{
		rawMsgKey:    msg,
		sourceKey:    source,
		timestampKey: time.Now().Format(time.RFC3339),
	}
}

type elasticConfig struct {
	Index             string   `json:"index"`
	Hosts             []string `json:"hosts"`
	Port              int      `json:"port"`
	Trace             bool     `json:"trace"`
	ReconnectAttempts int      `json:"reconnect_attempts"`
	RetrySeconds      int      `json:"retry_seconds"`
	BatchSize         int      `json:"batch_size"`
	BatchTimeoutSec   int      `json:"batch_timeout_sec"`
}

func (config elasticConfig) connectToES(log *logrus.Entry) (*elastigo.Conn, error) {
	log.WithFields(logrus.Fields{
		"hosts": config.Hosts,
		"index": config.Index,
		"port":  config.Port,
		"trace": config.Trace,
	}).Info("Connecting to elastic search")

	conn := elastigo.NewConn()
	if config.Port > 0 {
		conn.SetPort(fmt.Sprintf("%d", config.Port))
	}

	if config.Trace {
		conn.RequestTracer = func(method, url, body string) {
			log.WithFields(logrus.Fields{
				"component": "es",
				"method":    method,
				"url":       url,
				"trace":     true,
			}).Info(body)
		}
	}
	conn.Hosts = config.Hosts
	return conn, nil
}

func batchAndSend(config *elasticConfig, incoming <-chan *payload, stats *counters, log *logrus.Entry) {
	log = log.WithFields(logrus.Fields{
		"index": config.Index,
		"hosts": config.Hosts,
		"port":  config.Port,
	})

	log.WithFields(logrus.Fields{
		"batch_size":    config.BatchSize,
		"batch_timeout": config.BatchTimeoutSec,
	}).Info("Starting to consume forever and batch send to ES")

	batch := make([]*payload, 0, config.BatchSize)

	for {
		select {
		case in := <-incoming:
			batch = append(batch, in)
			if len(batch) >= config.BatchSize {
				log.Debug("Sending batch it sent the right size")
				go sendToES(config, log, stats, batch)
				batch = make([]*payload, 0, config.BatchSize)
			}
		case <-time.After(time.Duration(config.BatchTimeoutSec) * time.Second):
			log.Debug("Sending batch because of timeout")
			go sendToES(config, log, stats, batch)
			batch = make([]*payload, 0, config.BatchSize)
		}
	}
}

func sendToES(config *elasticConfig, log *logrus.Entry, stats *counters, batch []*payload) {
	if len(batch) == 0 {
		return
	}

	log = log.WithFields(logrus.Fields{
		"size":     len(batch),
		"batch_id": rand.Int(),
	})

	client, err := config.connectToES(log)
	if err != nil {
		log.WithError(err).Fatal("Failed to connect to elasticsearch")
	}
	log.Debug("Connected to elasticseach")
	indexer := client.NewBulkIndexerErrors(3, config.RetrySeconds)
	go logErrors(indexer, log)

	log.Debug("Started indexer")
	indexer.Start()
	defer func() {
		log.Debug("Shutting down indexer")
		indexer.Flush()
		indexer.Stop()
	}()

	for _, in := range batch {
		payload := *in
		resend := true
		for resend {
			resend = false
			log.Debugf("Sending to ES: %s", payload)

			now := time.Now()
			err := indexer.Index(
				config.Index, // index
				"log_line",   // _type
				"",           // _id
				"",           // parent
				"",           // ttl
				&now,         // _timestamp
				payload,
			)
			if err != nil {
				log.WithError(err).Warn("Error sending data to elasticsearch -- retrying")
				client = reconnect(log, config)
				resend = true
			} else {
				log.Debug("Sent")
				stats.esSent++
			}
		}
	}
}

func reconnect(log *logrus.Entry, config *elasticConfig) *elastigo.Conn {
	times := 0
	for ; times < config.ReconnectAttempts; times++ {
		log.Debugf("reconnecting attempt %d/%d", times+1, config.ReconnectAttempts)
		client, err := config.connectToES(log)
		if err == nil {
			log.Infof("Reconnected after %d attempts", times+1)
			return client
		}

		log.WithError(err).Warn("Failed to reconnect attempt %d", times+1)
	}
	log.Fatalf("Failed to reconnect to elasticsearch after %d attempts", config.ReconnectAttempts)
	return nil
}

func logErrors(indexer *elastigo.BulkIndexer, log *logrus.Entry) {
	for errBuf := range indexer.ErrorChannel {
		log.WithError(errBuf.Err).Warn("Trouble sending message to ES")
	}
}