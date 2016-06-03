package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/nats-io/nats"
	"github.com/spf13/cobra"

	"github.com/netlify/messaging"
)

var rootLog *logrus.Entry

type counters struct {
	natsConsumed int64
	esSent       int64
}

func main() {
	var cfgFile string
	rootCmd := cobra.Command{
		Short: "elastinat",
		Long:  "elastinat",
		Run: func(cmd *cobra.Command, args []string) {
			run(cfgFile)
		},
	}

	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "config.json", "the json config file")

	if err := rootCmd.Execute(); err != nil {
		if rootLog != nil {
			rootLog.WithError(err).Warn("Failed to execute command")
		}
		os.Exit(1)
	}
}

func run(configFile string) {
	config := new(configuration)
	err := loadFromFile(configFile, config)
	if err != nil {
		log.Fatalf("Failed to load configuation: %s %v", configFile, err)
	}

	rootLog, err = configureLogging(&config.LogConf)
	if err != nil {
		log.Fatalf("Failed to configure logging")
	}

	rootLog.Info("Configured - starting to connect and consume")

	// connect to ES
	clientChannel := make(chan *payload)
	stats := new(counters)
	go reportStats(config.ReportSec, stats, rootLog)

	go batchAndSend(&config.ElasticConf, clientChannel, stats, rootLog)

	// connect to NATS
	rootLog.WithFields(config.NatsConf.LogFields()).Info("Connecting to Nats")
	nc, err := messaging.ConnectToNats(&config.NatsConf)
	if err != nil {
		rootLog.WithError(err).Fatal("Failed to connect to nats")
	}

	// build all the tailers
	wg := sync.WaitGroup{}
	funcs := make([]func(), 0, len(config.Subjects))
	for _, pair := range config.Subjects {
		log := rootLog.WithFields(logrus.Fields{
			"subject": pair.Subject,
			"group":   pair.Group,
		})
		log.Debug("Connecting channel")

		var err error
		var sub *nats.Subscription
		if pair.Group == "" {
			sub, err = nc.Subscribe(pair.Subject, processMsg)
		} else {
			sub, err = nc.QueueSubscribe(pair.Subject, pair.Group, processMsg)
		}
		if err != nil {
			log.WithError(err).Fatal("Failed to subscribe")
		}

		wg.Add(1)
		f := func() {
			log.Info("Starting to consume")
			err := consumeForever(sub, clientChannel, stats)
			if err != nil {
				log.WithError(err).Warn("Problem while consuming messages")
			}
			log.Info("Finished consuming")
			wg.Done()
		}

		funcs = append(funcs, f)
	}

	// launch all the tailers
	for _, f := range funcs {
		go f()
	}

	wg.Wait()
	rootLog.Info("Shutting down")
}

func consumeForever(sub *nats.Subscription, toSend chan<- *payload, stats *counters) error {
	for {
		m, err := sub.NextMsg(time.Hour * 12)
		if err != nil {
			if err != nats.ErrTimeout {
				return err
			}
		}

		// DO NOT BLOCK
		// nats is truely a fire and forget, we need to get make sure we are ready to
		// take off the subject immediately. And we can have tons of go routines so
		// this seems like the natural pairing.
		go func() {
			payload := newPayload(string(m.Data), m.Subject)

			// maybe it is json!
			_ = json.Unmarshal(m.Data, payload)

			toSend <- payload
		}()
	}
}

func reportStats(reportSec int64, stats *counters, log *logrus.Entry) {
	if reportSec == 0 {
		log.Debug("Stats reporting disabled")
		return
	}

	ticks := time.Tick(time.Second * time.Duration(reportSec))
	for range ticks {
		log.WithFields(logrus.Fields{
			"messages_rx": stats.natsConsumed,
			"messages_tx": stats.esSent,
		}).Info("processed messages from nats to es")
	}
}
