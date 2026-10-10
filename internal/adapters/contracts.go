package adapters

import (
	"context"

	"go2tv.app/go2tv/v2/castprotocol"
	"go2tv.app/go2tv/v2/devices"
	"go2tv.app/go2tv/v2/soapcalls"
)

// Discovery provides LAN hardware discovery primitives.
type Discovery interface {
	StartDiscovery(ctx context.Context)
	LoadAllDevices() ([]devices.Device, error)
}

// CastClient represents a controllable Chromecast session.
type CastClient interface {
	ConnectContext(context.Context) error
	LoadMediaContext(context.Context, castprotocol.LoadRequest) error
	LoadMediaOnExisting(castprotocol.LoadRequest) error
	ForceClose() error
	Play() error
	Pause() error
	Seek(seconds int) error
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
	SendtoTVContext(context.Context, string) error
	StopPlayback() error
	SeekSoapCall(reltime string) error
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
