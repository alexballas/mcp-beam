package go2tv

import (
	"context"

	"github.com/alex/mcp-beam/internal/adapters"
	"go2tv.app/go2tv/v2/castprotocol"
	"go2tv.app/go2tv/v2/devices"
	"go2tv.app/go2tv/v2/soapcalls"
)

// Bundle wires all external go2tv-backed adapters in one place.
type Bundle struct {
	Discovery   adapters.Discovery
	CastFactory adapters.CastFactory
	DLNAFactory adapters.DLNAFactory
}

func NewBundle() Bundle {
	return Bundle{
		Discovery:   DiscoveryAdapter{},
		CastFactory: CastFactory{},
		DLNAFactory: DLNAFactory{},
	}
}

type DiscoveryAdapter struct{}

func (DiscoveryAdapter) StartChromecastDiscoveryLoop(ctx context.Context) {
	devices.StartChromecastDiscoveryLoop(ctx)
}

func (DiscoveryAdapter) LoadAllDevices(delaySeconds int) ([]devices.Device, error) {
	return devices.LoadAllDevices(delaySeconds)
}

type CastFactory struct{}

func (CastFactory) NewCastClient(deviceAddr string) (adapters.CastClient, error) {
	client, err := castprotocol.NewCastClient(deviceAddr)
	if err != nil {
		return nil, err
	}

	return &CastClientAdapter{client: client}, nil
}

type CastClientAdapter struct {
	client *castprotocol.CastClient
}

func (c *CastClientAdapter) Connect() error {
	return c.client.Connect()
}

func (c *CastClientAdapter) Load(mediaURL, contentType string, startTime int, duration float64, subtitleURL string, live bool) error {
	return c.client.Load(mediaURL, contentType, startTime, duration, subtitleURL, live)
}

func (c *CastClientAdapter) Stop() error {
	return c.client.Stop()
}

func (c *CastClientAdapter) GetStatus() (*castprotocol.CastStatus, error) {
	return c.client.GetStatus()
}

func (c *CastClientAdapter) Close(stopMedia bool) error {
	return c.client.Close(stopMedia)
}

type DLNAFactory struct{}

func (DLNAFactory) NewTVPayload(o *soapcalls.Options) (adapters.DLNAPayload, error) {
	payload, err := soapcalls.NewTVPayload(o)
	if err != nil {
		return nil, err
	}

	return &DLNAPayloadAdapter{payload: payload}, nil
}

type DLNAPayloadAdapter struct {
	payload *soapcalls.TVPayload
}

func (d *DLNAPayloadAdapter) SendtoTV(action string) error {
	return d.payload.SendtoTV(action)
}

func (d *DLNAPayloadAdapter) GetTransportInfo() ([]string, error) {
	return d.payload.GetTransportInfo()
}

func (d *DLNAPayloadAdapter) GetPositionInfo() ([]string, error) {
	return d.payload.GetPositionInfo()
}

func (d *DLNAPayloadAdapter) ListenAddress() string {
	return d.payload.ListenAddress()
}

func (d *DLNAPayloadAdapter) SetContext(ctx context.Context) {
	d.payload.SetContext(ctx)
}

func (d *DLNAPayloadAdapter) MediaURL() string {
	return d.payload.MediaURL
}

func (d *DLNAPayloadAdapter) SetMediaURL(mediaURL string) {
	d.payload.MediaURL = mediaURL
}

func (d *DLNAPayloadAdapter) RawPayload() *soapcalls.TVPayload {
	return d.payload
}

var (
	_ adapters.Discovery   = DiscoveryAdapter{}
	_ adapters.CastFactory = CastFactory{}
	_ adapters.DLNAFactory = DLNAFactory{}
)
