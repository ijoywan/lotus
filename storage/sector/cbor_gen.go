package sector

import (
	"fmt"
	"io"

	cbg "github.com/whyrusleeping/cbor-gen"
	xerrors "golang.org/x/xerrors"
)

/* This file was generated by github.com/whyrusleeping/cbor-gen */

var _ = xerrors.Errorf

func (t *DealMapping) MarshalCBOR(w io.Writer) error {
	if t == nil {
		_, err := w.Write(cbg.CborNull)
		return err
	}
	if _, err := w.Write([]byte{131}); err != nil {
		return err
	}

	// t.t.DealIDs ([]uint64) (slice)
	if _, err := w.Write(cbg.CborEncodeMajorType(cbg.MajArray, uint64(len(t.DealIDs)))); err != nil {
		return err
	}
	for _, v := range t.DealIDs {
		if err := cbg.CborWriteHeader(w, cbg.MajUnsignedInt, v); err != nil {
			return err
		}
	}

	// t.t.Allocated (uint64) (uint64)
	if _, err := w.Write(cbg.CborEncodeMajorType(cbg.MajUnsignedInt, uint64(t.Allocated))); err != nil {
		return err
	}

	// t.t.Committed (bool) (bool)
	if err := cbg.WriteBool(w, t.Committed); err != nil {
		return err
	}
	return nil
}

func (t *DealMapping) UnmarshalCBOR(r io.Reader) error {
	br := cbg.GetPeeker(r)

	maj, extra, err := cbg.CborReadHeader(br)
	if err != nil {
		return err
	}
	if maj != cbg.MajArray {
		return fmt.Errorf("cbor input should be of type array")
	}

	if extra != 3 {
		return fmt.Errorf("cbor input had wrong number of fields")
	}

	// t.t.DealIDs ([]uint64) (slice)

	maj, extra, err = cbg.CborReadHeader(br)
	if err != nil {
		return err
	}
	if extra > 8192 {
		return fmt.Errorf("t.DealIDs: array too large (%d)", extra)
	}

	if maj != cbg.MajArray {
		return fmt.Errorf("expected cbor array")
	}
	if extra > 0 {
		t.DealIDs = make([]uint64, extra)
	}
	for i := 0; i < int(extra); i++ {

		maj, val, err := cbg.CborReadHeader(br)
		if err != nil {
			return xerrors.Errorf("failed to read uint64 for t.DealIDs slice: %w", err)
		}

		if maj != cbg.MajUnsignedInt {
			return xerrors.Errorf("value read for array t.DealIDs was not a uint, instead got %d", maj)
		}

		t.DealIDs[i] = val
	}

	// t.t.Allocated (uint64) (uint64)

	maj, extra, err = cbg.CborReadHeader(br)
	if err != nil {
		return err
	}
	if maj != cbg.MajUnsignedInt {
		return fmt.Errorf("wrong type for uint64 field")
	}
	t.Allocated = uint64(extra)
	// t.t.Committed (bool) (bool)

	maj, extra, err = cbg.CborReadHeader(br)
	if err != nil {
		return err
	}
	if maj != cbg.MajOther {
		return fmt.Errorf("booleans must be major type 7")
	}
	switch extra {
	case 20:
		t.Committed = false
	case 21:
		t.Committed = true
	default:
		return fmt.Errorf("booleans are either major type 7, value 20 or 21 (got %d)", extra)
	}
	return nil
}
