package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/opensourceways/community-robot-lib/config"
	"github.com/opensourceways/community-robot-lib/interrupts"
	"github.com/opensourceways/community-robot-lib/kafka"
	"github.com/opensourceways/community-robot-lib/logrusutil"
	"github.com/opensourceways/community-robot-lib/mq"
	"github.com/opensourceways/community-robot-lib/utils"
	"github.com/sirupsen/logrus"

	"github.com/opensourceways/sync-repo-file/client"
	"github.com/opensourceways/sync-repo-file/server"
)

type options struct {
	configFile     string
	startTime      string
	topic          string
	interval       int
	concurrentSize int
}

func (o options) validate() error {
	if _, _, err := o.parseStartTime(); err != nil {
		return err
	}

	if o.topic == "" {
		return fmt.Errorf("topic must be set")
	}

	return nil
}

func (o options) parseStartTime() (int, int, error) {
	format := "2006-01-02T"
	t, err := time.Parse(format+"15:04", format+o.startTime)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid start_time: %s", err.Error())
	}

	return t.Hour(), t.Minute(), nil
}

func (o options) getStartTime() time.Duration {
	seconds := func(h, m int) int {
		return h*3600 + m*60
	}

	now := time.Now()
	t0 := seconds(now.Hour(), now.Minute())

	h, m, _ := o.parseStartTime()
	t1 := seconds(h, m)

	diff := t1 - t0
	if diff >= 0 {
		return time.Duration(diff) * time.Second
	}
	return time.Duration(seconds(24, 0)+diff) * time.Second
}

func gatherOptions(fs *flag.FlagSet, args ...string) options {
	var o options

	fs.StringVar(&o.configFile, "config-file", "", "Path to the config file.")
	fs.StringVar(&o.startTime, "start-time", "01:00", "Time to synchronize repo file for the first time. The format is Hour:Minute")
	fs.StringVar(&o.topic, "topic", "", "The topic to which jobs need to be published ")
	fs.IntVar(&o.interval, "interval", 24, "Interval between two synchronizations. The unit is hour")
	fs.IntVar(&o.concurrentSize, "concurrent-size", 500, "The concurrent size for synchronizing files of repo branch.")

	fs.Parse(args)
	return o
}

func main() {
	logrusutil.ComponentInit("sync-repo-file-trigger")

	o := gatherOptions(flag.NewFlagSet(os.Args[0], flag.ExitOnError), os.Args[1:]...)
	if err := o.validate(); err != nil {
		logrus.WithError(err).Error("parse option")
		return
	}

	agent := config.NewConfigAgent(func() config.Config {
		return new(configuration)
	})
	if err := agent.Start(o.configFile); err != nil {
		logrus.WithError(err).Error("start config file agent")
		return
	}

	clients := map[string]*client.SyncFileClient{}

	task := func() {
		_, v := agent.GetConfig()
		cfg := v.(*configuration)

		for _, item := range cfg.Clients {
			if _, ok := clients[item.Platform]; !ok {
				cli, err := client.NewSyncFileClient(item.Endpoint)
				if err != nil {
					logrus.WithField("enpoint", item.Endpoint).WithError(err).Infof("init sync file client")
				} else {
					clients[item.Platform] = cli
				}
			}
		}

		clis := map[string]server.SyncFileClient{}
		for k, v := range clients {
			clis[k] = v
		}
		logrus.Info("start")
		server.DoOnce(clis, cfg.SyncFiles, o.concurrentSize, o.topic)
	}

	go task()

	if err := initBroker(&agent); err != nil {
		logrus.WithError(err).Fatal("Error init broker")
	}

	t := utils.NewTimer()

	t.Start(func() { go task() }, time.Duration(o.interval)*time.Hour, o.getStartTime())

	defer interrupts.WaitForGracefulShutdown()

	ch := make(chan struct{})

	interrupts.OnInterrupt(func() {
		t.Stop()
		for _, item := range clients {
			item.Stop()
		}
		close(ch)

		_ = kafka.Disconnect()
	})

	<-ch
}

func initBroker(agent *config.ConfigAgent) error {
	cfg := new(configuration)
	_, v := agent.GetConfig()
	if c, ok := v.(*configuration); ok {
		cfg = c
	}

	tlsConfig, err := cfg.MQConfig.TLSConfig.TLSConfig()
	if err != nil {
		return err
	}

	err = kafka.Init(
		mq.Addresses(cfg.MQConfig.Addresses...),
		mq.SetTLSConfig(tlsConfig),
		mq.Log(logrus.WithField("module", "broker")),
	)

	if err != nil {
		return err
	}

	return kafka.Connect()
}
