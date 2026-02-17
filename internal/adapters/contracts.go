package adapters

import (
	"context"

	"go2tv.app/go2tv/v2/castprotocol"
	"go2tv.app/go2tv/v2/devices"
	"go2tv.app/go2tv/v2/soapcalls"
)

// Discovery provides LAN hardware discovery primitives.
type Discovery interface {
	StartChromecastDiscoveryLoop(ctx context.Context)
	LoadAllDevices(delaySeconds int) ([]devices.Device, error)
}

// CastClient represents a controllable Chromecast session.
type CastClient interface {
	Connect() error
	Load(mediaURL, contentType string, startTime int, duration float64, subtitleURL string, live bool) error
	Stop() error
	GetStatus() (*castprotocol.CastStatus, error)
	Close(stopMedia bool) error
}

// CastFactory creates CastClient instances.
type CastFactory interface {
	NewCastClient(deviceAddr string) (CastClient, error)
}

// DLNAPayload represents a DLNA control channel.
type DLNAPayload interface {
	SendtoTV(action string) error
	GetTransportInfo() ([]string, error)
	GetPositionInfo() ([]string, error)
	ListenAddress() string
	SetContext(ctx context.Context)
	MediaURL() string
	SetMediaURL(mediaURL string)
	RawPayload() *soapcalls.TVPayload
}

// DLNAFactory creates DLNA payload/controller instances.
type DLNAFactory interface {
	NewTVPayload(o *soapcalls.Options) (DLNAPayload, error)
}
