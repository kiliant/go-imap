package imapclient

import (
	"github.com/kiliant/go-imap"
	"github.com/kiliant/go-imap/internal/imapcodec"
)

// responseData is decoded once on the reader goroutine, before command
// collectors choose whether they own the response. A collector can inspect it
// and decline without advancing the shared decoder for the next collector.
// FETCH deliberately stays on its existing streaming path.
type responseData struct {
	decoded    bool
	err        error
	status     *StatusData
	list       *ListData
	esearch    *esearchResponse
	metadata   *metadataResponse
	uidbatches *imap.UIDBatchesData
}

func (resp *untaggedResponse) decodeData() error {
	if resp.data.decoded || resp.hasNum || resp.cond != nil {
		return resp.data.err
	}
	switch resp.name {
	case "UIDBATCHES":
		resp.data.uidbatches, resp.data.err = imapcodec.ReadUIDBatchesResponse(resp.dec)
	case "STATUS":
		resp.data.status, resp.data.err = readStatusResponse(resp.dec)
	case "LIST", "LSUB", "XLIST":
		resp.data.list, resp.data.err = readListResponse(resp.dec)
	case "ESEARCH":
		resp.data.esearch, resp.data.err = readESearchResponse(resp.dec)
	case "METADATA":
		resp.data.metadata, resp.data.err = readMetadataData(resp.dec)
	default:
		return nil
	}
	resp.data.decoded = true
	return resp.data.err
}
