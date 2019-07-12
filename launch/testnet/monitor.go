package main

import (
	log "github.com/sirupsen/logrus"
	cfg "gitlab.dusk.network/dusk-core/dusk-go/pkg/config"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/eventmon/monitor"
	"gitlab.dusk.network/dusk-core/dusk-go/pkg/p2p/wire"
)

func ConnectToLogMonitor(bus wire.EventBroker) error {
	if cfg.Get().General.Network == "testnet" && cfg.Get().Logger.Monitor.Enabled {
		monitorUrl := cfg.Get().Logger.Monitor.Target
		log.Infof("Connecting to log reserved monitoring file on %v\n", monitorUrl)
		supervisor, err := monitor.Launch(bus, monitorUrl)
		if err != nil {
			//TODO: there should maybe be something that uses the supervisor
			return err
		}
		if cfg.Get().Logger.Monitor.StreamErrors {
			log.AddHook(supervisor)
		}
	}

	return nil
}