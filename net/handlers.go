package net

import (
	"bytes"

	"github.com/sahib/brig/net/capnp"
)

func (hdl *handler) FetchStore(call capnp.Sync_fetchStore) error {
	user := hdl.rp.CurrentUser()
	fs, err := hdl.rp.FS(user, hdl.bk)
	if err != nil {
		return err
	}

	buf := &bytes.Buffer{}
	if err := fs.Export(buf); err != nil {
		return err
	}

	return call.Results.SetData(buf.Bytes())
}

func (hdl *handler) Ping(call capnp.Meta_ping) error {
	return call.Results.SetReply("ALIVE")
}

func (hdl *handler) Version(call capnp.API_version) error {
	call.Results.SetVersion(1)
	return nil
}
