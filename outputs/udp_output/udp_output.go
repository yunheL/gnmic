// © 2022 Nokia.
//
// This code is a Contribution to the gNMIc project (“Work”) made under the Google Software Grant and Corporate Contributor License Agreement (“CLA”) and governed by the Apache License 2.0.
// No other rights or licenses in or to any of Nokia’s intellectual property are granted for any other purpose.
// This code is provided on an “as is” basis without any warranties of any kind.
//
// SPDX-License-Identifier: Apache-2.0

package udp_output

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"text/template"
	"time"

	"github.com/openconfig/gnmic/formatters"
	"github.com/openconfig/gnmic/outputs"
	"github.com/openconfig/gnmic/types"
	"github.com/openconfig/gnmic/utils"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

const (
	defaultRetryTimer = 2 * time.Second
	loggingPrefix     = "[udp_output:%s] "
)

func init() {
	outputs.Register("udp", func() outputs.Output {
		return &UDPSock{
			Cfg:    &Config{},
			logger: log.New(io.Discard, loggingPrefix, utils.DefaultLoggingFlags),
		}
	})
}

type UDPSock struct {
	Cfg *Config

	conn     *net.UDPConn
	cancelFn context.CancelFunc
	buffer   chan []byte
	limiter  *time.Ticker
	logger   *log.Logger
	mo       *formatters.MarshalOptions
	evps     []formatters.EventProcessor

	targetTpl *template.Template
}

type Config struct {
	Address            string        `mapstructure:"address,omitempty"` // ip:port
	Rate               time.Duration `mapstructure:"rate,omitempty"`
	BufferSize         uint          `mapstructure:"buffer-size,omitempty"`
	Format             string        `mapstructure:"format,omitempty"`
	AddTarget          string        `mapstructure:"add-target,omitempty"`
	TargetTemplate     string        `mapstructure:"target-template,omitempty"`
	OverrideTimestamps bool          `mapstructure:"override-timestamps,omitempty"`
	SplitEvents        bool          `mapstructure:"split-events,omitempty"`
	RetryInterval      time.Duration `mapstructure:"retry-interval,omitempty"`
	EnableMetrics      bool          `mapstructure:"enable-metrics,omitempty"`
	EventProcessors    []string      `mapstructure:"event-processors,omitempty"`
}

func (u *UDPSock) SetLogger(logger *log.Logger) {
	if logger != nil && u.logger != nil {
		u.logger.SetOutput(logger.Writer())
		u.logger.SetFlags(logger.Flags())
	}
}

func (u *UDPSock) SetEventProcessors(ps map[string]map[string]interface{},
	logger *log.Logger,
	tcs map[string]*types.TargetConfig,
	acts map[string]map[string]interface{}) {
	for _, epName := range u.Cfg.EventProcessors {
		if epCfg, ok := ps[epName]; ok {
			epType := ""
			for k := range epCfg {
				epType = k
				break
			}
			if in, ok := formatters.EventProcessors[epType]; ok {
				ep := in()
				err := ep.Init(epCfg[epType], formatters.WithLogger(logger),
					formatters.WithTargets(tcs),
					formatters.WithActions(acts))
				if err != nil {
					u.logger.Printf("failed initializing event processor '%s' of type='%s': %v", epName, epType, err)
					continue
				}
				u.evps = append(u.evps, ep)
				u.logger.Printf("added event processor '%s' of type=%s to udp output", epName, epType)
				continue
			}
			u.logger.Printf("%q event processor has an unknown type=%q", epName, epType)
			continue
		}
		u.logger.Printf("%q event processor not found!", epName)
	}
}

func (u *UDPSock) Init(ctx context.Context, name string, cfg map[string]interface{}, opts ...outputs.Option) error {
	err := outputs.DecodeConfig(cfg, u.Cfg)
	if err != nil {
		return err
	}
	u.logger.SetPrefix(fmt.Sprintf(loggingPrefix, name))

	for _, opt := range opts {
		opt(u)
	}
	_, _, err = net.SplitHostPort(u.Cfg.Address)
	if err != nil {
		return fmt.Errorf("wrong address format: %v", err)
	}
	if u.Cfg.RetryInterval == 0 {
		u.Cfg.RetryInterval = defaultRetryTimer
	}

	u.buffer = make(chan []byte, u.Cfg.BufferSize)
	if u.Cfg.Rate > 0 {
		u.limiter = time.NewTicker(u.Cfg.Rate)
	}
	go func() {
		<-ctx.Done()
		u.Close()
	}()
	ctx, u.cancelFn = context.WithCancel(ctx)
	u.mo = &formatters.MarshalOptions{
		Format:     u.Cfg.Format,
		OverrideTS: u.Cfg.OverrideTimestamps,
	}
	if u.Cfg.TargetTemplate == "" {
		u.targetTpl = outputs.DefaultTargetTemplate
	} else if u.Cfg.AddTarget != "" {
		u.targetTpl, err = utils.CreateTemplate("target-template", u.Cfg.TargetTemplate)
		if err != nil {
			return err
		}
		u.targetTpl = u.targetTpl.Funcs(outputs.TemplateFuncs)
	}
	go u.start(ctx)
	return nil
}

func (u *UDPSock) Write(ctx context.Context, m proto.Message, meta outputs.Meta) {
	if m == nil {
		return
	}

	select {
	case <-ctx.Done():
		return
	default:
		rsp, err := outputs.AddSubscriptionTarget(m, meta, u.Cfg.AddTarget, u.targetTpl)
		if err != nil {
			u.logger.Printf("failed to add target to the response: %v", err)
		}
		bb, err := outputs.Marshal(rsp, meta, u.mo, u.Cfg.SplitEvents, u.evps...)
		if err != nil {
			u.logger.Printf("failed marshaling proto msg: %v", err)
			return
		}
		for _, b := range bb {
			u.buffer <- b
		}
	}
}

func (u *UDPSock) WriteEvent(ctx context.Context, ev *formatters.EventMsg) {}

func (u *UDPSock) Close() error {
	u.cancelFn()
	if u.limiter != nil {
		u.limiter.Stop()
	}
	return nil
}

func (u *UDPSock) RegisterMetrics(reg *prometheus.Registry) {}

func (u *UDPSock) String() string {
	b, err := json.Marshal(u)
	if err != nil {
		return ""
	}
	return string(b)
}

func (u *UDPSock) start(ctx context.Context) {
	var udpAddr *net.UDPAddr
	var err error
	defer u.Close()
DIAL:
	if ctx.Err() != nil {
		u.logger.Printf("context error: %v", ctx.Err())
		return
	}
	udpAddr, err = net.ResolveUDPAddr("udp", u.Cfg.Address)
	if err != nil {
		u.logger.Printf("failed to dial udp: %v", err)
		time.Sleep(u.Cfg.RetryInterval)
		goto DIAL
	}
	u.conn, err = net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		u.logger.Printf("failed to dial udp: %v", err)
		time.Sleep(u.Cfg.RetryInterval)
		goto DIAL
	}
	for {
		select {
		case <-ctx.Done():
			return
		case b := <-u.buffer:
			if u.limiter != nil {
				<-u.limiter.C
			}
			_, err = u.conn.Write(b)
			if err != nil {
				u.logger.Printf("failed sending udp bytes: %v", err)
				time.Sleep(u.Cfg.RetryInterval)
				goto DIAL
			}
		}
	}
}

func (u *UDPSock) SetName(name string)                             {}
func (u *UDPSock) SetClusterName(name string)                      {}
func (u *UDPSock) SetTargetsConfig(map[string]*types.TargetConfig) {}
