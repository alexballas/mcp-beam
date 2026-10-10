package go2tv

import (
	"context"

	"go2tv.app/go2tv/v2/castprotocol"
	"go2tv.app/go2tv/v2/devices"
	"go2tv.app/go2tv/v2/soapcalls"
	"go2tv.app/mcp-beam/internal/adapters"
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

func (DiscoveryAdapter) StartDiscovery(ctx context.Context) {
	devices.StartDiscovery(ctx)
}

func (DiscoveryAdapter) LoadAllDevices() ([]devices.Device, error) {
	return devices.LoadAllDevices()
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

func (c *CastClientAdapter) ConnectContext(ctx context.Context) error {
	return c.client.ConnectContext(ctx)
}

func (c *CastClientAdapter) LoadMediaContext(ctx context.Context, req castprotocol.LoadRequest) error {
	return c.client.LoadMediaContext(ctx, req)
}

func (c *CastClientAdapter) LoadMediaOnExisting(req castprotocol.LoadRequest) error {
	return c.client.LoadMediaOnExisting(req)
}

func (c *CastClientAdapter) ForceClose() error {
	return c.client.ForceClose()
}

func (c *CastClientAdapter) Play() error {
	return c.client.Play()
}

func (c *CastClientAdapter) Pause() error {
	return c.client.Pause()
}

func (c *CastClientAdapter) Seek(seconds int) error {
	return c.client.Seek(seconds)
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

func (d *DLNAPayloadAdapter) SendtoTVContext(ctx context.Context, action string) error {
	return d.payload.SendtoTVContext(ctx, action)
}

func (d *DLNAPayloadAdapter) StopPlayback() error {
	return d.payload.PlayPauseStopSoapCall("Stop")
}

func (d *DLNAPayloadAdapter) SeekSoapCall(reltime string) error {
	return d.payload.SeekSoapCall(reltime)
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
