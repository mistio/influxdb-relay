package relay

import (
	"fmt"
	"sync"

	log "github.com/golang/glog"
)

type Service struct {
	relays map[string]Relay
}

func New(config Config) (*Service, error) {
	s := new(Service)
	s.relays = make(map[string]Relay)

	for _, cfg := range config.HTTPRelays {
		h, err := NewHTTP(cfg)
		if err != nil {
			return nil, err
		}
		if s.relays[h.Name()] != nil {
			return nil, fmt.Errorf("duplicate relay: %q", h.Name())
		}
		s.relays[h.Name()] = h
	}

	for _, cfg := range config.UDPRelays {
		u, err := NewUDP(cfg)
		if err != nil {
			return nil, err
		}
		if s.relays[u.Name()] != nil {
			return nil, fmt.Errorf("duplicate relay: %q", u.Name())
		}
		s.relays[u.Name()] = u
	}

	for _, cfg := range config.BeringeiRelays {
		b, err := NewBeringei(cfg)
		if err != nil {
			return nil, err
		}
		if s.relays[b.Name()] != nil {
			return nil, fmt.Errorf("duplicate relay: %q", b.Name())
		}
		s.relays[b.Name()] = b
	}

	for _, cfg := range config.GraphiteRelays {
		g, err := NewGraphiteRelay(cfg)
		if err != nil {
			return nil, err
		}
		if s.relays[g.Name()] != nil {
			return nil, fmt.Errorf("duplicate relay: %q", g.Name())
		}
		s.relays[g.Name()] = g

	}
	return s, nil
}

func (s *Service) Run() {
	var wg sync.WaitGroup
	wg.Add(len(s.relays))

	for k := range s.relays {
		relay := s.relays[k]
		go func() {
			defer wg.Done()

			if err := relay.Run(); err != nil {
				log.Error("Error running relay %q: %v", relay.Name(), err)
			}
		}()
	}

	wg.Wait()
}

func (s *Service) Stop() {
	for _, v := range s.relays {
		v.Stop()
	}
}

type Relay interface {
	Name() string
	Run() error
	Stop() error
}
