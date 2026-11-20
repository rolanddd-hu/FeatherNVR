package fmp4

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

const maxBoxSize = 64 * 1024 * 1024 // 64 MB safety cap per ISOBMFF box

// Box represents a single raw ISOBMFF box read from a stream.
type Box struct {
	Type string
	Raw  []byte
}

// InitInfo holds parsed initialization metadata from an ftyp + moov pair.
type InitInfo struct {
	Raw          []byte
	VideoTrackID uint32
	Timescales   map[uint32]uint32
	Width        uint32
	Height       uint32
	Codec        string
}

// Fragment represents a single moof + mdat media fragment.
type Fragment struct {
	Moof       []byte
	Mdat       []byte
	TrackID    uint32
	IsVideo    bool
	IsKeyframe bool
	DTS        uint64
	Duration   uint32
	ReceivedAt time.Time
}

// ByteSize returns the combined byte size of moof + mdat.
func (f Fragment) ByteSize() int64 {
	return int64(len(f.Moof) + len(f.Mdat))
}

// ReadBox reads the next complete ISOBMFF box from r.
func ReadBox(r io.Reader) (Box, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Box{}, err
	}

	size32 := binary.BigEndian.Uint32(hdr[0:4])
	typ := string(hdr[4:8])

	var boxSize uint64
	var headerLen int
	var extHdr [8]byte

	switch size32 {
	case 1:
		if _, err := io.ReadFull(r, extHdr[:]); err != nil {
			return Box{}, err
		}
		boxSize = binary.BigEndian.Uint64(extHdr[:])
		headerLen = 16
	case 0:
		return Box{}, fmt.Errorf("box %q has unbounded size 0 in live stream", typ)
	default:
		boxSize = uint64(size32)
		headerLen = 8
	}

	if boxSize < uint64(headerLen) || boxSize > maxBoxSize {
		return Box{}, fmt.Errorf("box %q invalid size %d", typ, boxSize)
	}

	raw := make([]byte, int(boxSize))
	copy(raw[:8], hdr[:])
	if headerLen == 16 {
		copy(raw[8:16], extHdr[:])
	}
	if _, err := io.ReadFull(r, raw[headerLen:]); err != nil {
		return Box{}, err
	}

	return Box{Type: typ, Raw: raw}, nil
}

type childBox struct {
	typ     string
	offset  int // offset of box start within parent slice
	hdrLen  int
	payload int // offset of box payload within parent slice
	end     int // end offset of box within parent slice
}

func iterBoxes(buf []byte, start, end int, fn func(cb childBox) error) error {
	pos := start
	for pos < end {
		if pos+8 > end {
			return fmt.Errorf("truncated box header at offset %d", pos)
		}
		size32 := binary.BigEndian.Uint32(buf[pos : pos+4])
		typ := string(buf[pos+4 : pos+8])
		hdrLen := 8
		var size int

		if size32 == 1 {
			if pos+16 > end {
				return fmt.Errorf("truncated 64-bit box header for %q", typ)
			}
			s64 := binary.BigEndian.Uint64(buf[pos+8 : pos+16])
			if s64 < 16 || s64 > uint64(end-pos) {
				return fmt.Errorf("invalid 64-bit size %d for %q", s64, typ)
			}
			hdrLen = 16
			size = int(s64)
		} else if size32 == 0 {
			size = end - pos
		} else {
			if size32 < 8 || int(size32) > end-pos {
				return fmt.Errorf("invalid size %d for %q", size32, typ)
			}
			size = int(size32)
		}

		cb := childBox{
			typ:     typ,
			offset:  pos,
			hdrLen:  hdrLen,
			payload: pos + hdrLen,
			end:     pos + size,
		}
		if err := fn(cb); err != nil {
			return err
		}
		pos += size
	}
	return nil
}

func boxHeaderLen(raw []byte) (int, error) {
	if len(raw) < 8 {
		return 0, errors.New("box shorter than 8 bytes")
	}
	if binary.BigEndian.Uint32(raw[0:4]) == 1 {
		if len(raw) < 16 {
			return 0, errors.New("64-bit box shorter than 16 bytes")
		}
		return 16, nil
	}
	return 8, nil
}

// ParseInit parses ftyp and moov boxes to identify the video track and track timescales.
func ParseInit(ftyp, moov []byte) (*InitInfo, error) {
	moovHdr, err := boxHeaderLen(moov)
	if err != nil {
		return nil, fmt.Errorf("moov header: %w", err)
	}
	if string(moov[4:8]) != "moov" {
		return nil, fmt.Errorf("expected moov box, got %q", string(moov[4:8]))
	}

	raw := make([]byte, len(ftyp)+len(moov))
	copy(raw, ftyp)
	copy(raw[len(ftyp):], moov)

	info := &InitInfo{
		Raw:        raw,
		Timescales: make(map[uint32]uint32),
	}

	err = iterBoxes(moov, moovHdr, len(moov), func(cb childBox) error {
		if cb.typ != "trak" {
			return nil
		}
		var (
			trackID    uint32
			timescale  uint32
			handler    string
			tkhdW      uint32
			tkhdH      uint32
			stsdW      uint32
			stsdH      uint32
			sampleType string
		)

		if err := iterBoxes(moov, cb.payload, cb.end, func(tc childBox) error {
			switch tc.typ {
			case "tkhd":
				p := moov[tc.payload:tc.end]
				if len(p) < 1 {
					return nil
				}
				switch p[0] {
				case 0:
					if len(p) >= 16 {
						trackID = binary.BigEndian.Uint32(p[12:16])
					}
					if len(p) >= 84 {
						tkhdW = uint32(binary.BigEndian.Uint16(p[76:78]))
						tkhdH = uint32(binary.BigEndian.Uint16(p[80:82]))
					}
				case 1:
					if len(p) >= 24 {
						trackID = binary.BigEndian.Uint32(p[20:24])
					}
					if len(p) >= 96 {
						tkhdW = uint32(binary.BigEndian.Uint16(p[88:90]))
						tkhdH = uint32(binary.BigEndian.Uint16(p[92:94]))
					}
				}
			case "mdia":
				return iterBoxes(moov, tc.payload, tc.end, func(mc childBox) error {
					switch mc.typ {
					case "mdhd":
						p := moov[mc.payload:mc.end]
						if len(p) < 1 {
							return nil
						}
						switch p[0] {
						case 0:
							if len(p) >= 16 {
								timescale = binary.BigEndian.Uint32(p[12:16])
							}
						case 1:
							if len(p) >= 24 {
								timescale = binary.BigEndian.Uint32(p[20:24])
							}
						}
					case "hdlr":
						p := moov[mc.payload:mc.end]
						if len(p) >= 12 {
							handler = string(p[8:12])
						}
					case "minf":
						return iterBoxes(moov, mc.payload, mc.end, func(mfc childBox) error {
							if mfc.typ != "stbl" {
								return nil
							}
							return iterBoxes(moov, mfc.payload, mfc.end, func(sc childBox) error {
								if sc.typ != "stsd" || sc.end-sc.payload < 8 {
									return nil
								}
								return iterBoxes(moov, sc.payload+8, sc.end, func(entry childBox) error {
									if sampleType == "" {
										sampleType = entry.typ
										ep := moov[entry.payload:entry.end]
										if len(ep) >= 28 {
											stsdW = uint32(binary.BigEndian.Uint16(ep[24:26]))
											stsdH = uint32(binary.BigEndian.Uint16(ep[26:28]))
										}
									}
									return nil
								})
							})
						})
					}
					return nil
				})
			}
			return nil
		}); err != nil {
			return err
		}

		if trackID != 0 {
			if timescale == 0 {
				timescale = 90000
			}
			info.Timescales[trackID] = timescale
			if handler == "vide" && info.VideoTrackID == 0 {
				info.VideoTrackID = trackID
				if stsdW > 0 && stsdH > 0 {
					info.Width = stsdW
					info.Height = stsdH
				} else if tkhdW > 0 && tkhdH > 0 {
					info.Width = tkhdW
					info.Height = tkhdH
				}
				info.Codec = mapSampleEntryCodec(sampleType)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse moov: %w", err)
	}

	return info, nil
}

func mapSampleEntryCodec(fourCC string) string {
	switch fourCC {
	case "avc1", "avc3":
		return "H.264"
	case "hev1", "hvc1":
		return "H.265"
	case "mp4v":
		return "MPEG-4"
	case "vp09":
		return "VP9"
	case "av01":
		return "AV1"
	default:
		return fourCC
	}
}

// ParseMoof parses metadata from a moof box using track info from InitInfo.
func ParseMoof(moofRaw []byte, init *InitInfo) (Fragment, error) {
	moofHdr, err := boxHeaderLen(moofRaw)
	if err != nil {
		return Fragment{}, fmt.Errorf("moof header: %w", err)
	}
	if string(moofRaw[4:8]) != "moof" {
		return Fragment{}, fmt.Errorf("expected moof box, got %q", string(moofRaw[4:8]))
	}

	var best Fragment
	found := false

	err = iterBoxes(moofRaw, moofHdr, len(moofRaw), func(cb childBox) error {
		if cb.typ != "traf" {
			return nil
		}

		var trackID uint32
		var defaultDur uint32
		var defaultFlags uint32
		var hasDefaultFlags bool
		var dts uint64
		var totalDur uint32
		var sample0Flags uint32
		var hasSample0Flags bool

		err := iterBoxes(moofRaw, cb.payload, cb.end, func(tc childBox) error {
			p := moofRaw[tc.payload:tc.end]
			switch tc.typ {
			case "tfhd":
				if len(p) < 8 {
					return errors.New("truncated tfhd")
				}
				flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
				trackID = binary.BigEndian.Uint32(p[4:8])
				off := 8
				if flags&0x000001 != 0 { // base-data-offset-present
					off += 8
				}
				if flags&0x000002 != 0 { // sample-description-index-present
					off += 4
				}
				if flags&0x000008 != 0 && off+4 <= len(p) { // default-sample-duration-present
					defaultDur = binary.BigEndian.Uint32(p[off : off+4])
					off += 4
				}
				if flags&0x000010 != 0 && off+4 <= len(p) { // default-sample-size-present
					off += 4
				}
				if flags&0x000020 != 0 && off+4 <= len(p) { // default-sample-flags-present
					defaultFlags = binary.BigEndian.Uint32(p[off : off+4])
					hasDefaultFlags = true
					off += 4
				}
			case "tfdt":
				if len(p) < 8 {
					return errors.New("truncated tfdt")
				}
				if p[0] == 0 {
					dts = uint64(binary.BigEndian.Uint32(p[4:8]))
				} else if len(p) >= 12 {
					dts = binary.BigEndian.Uint64(p[4:12])
				}
			case "trun":
				if len(p) < 8 {
					return errors.New("truncated trun")
				}
				flags := uint32(p[1])<<16 | uint32(p[2])<<8 | uint32(p[3])
				count := binary.BigEndian.Uint32(p[4:8])
				off := 8
				if flags&0x000001 != 0 { // data-offset-present
					off += 4
				}
				var firstFlags uint32
				hasFirstFlags := false
				if flags&0x000004 != 0 && off+4 <= len(p) { // first-sample-flags-present
					firstFlags = binary.BigEndian.Uint32(p[off : off+4])
					hasFirstFlags = true
					off += 4
				}

				for i := uint32(0); i < count; i++ {
					sDur := defaultDur
					sFlags := defaultFlags
					hasSFlags := hasDefaultFlags
					if i == 0 && hasFirstFlags {
						sFlags = firstFlags
						hasSFlags = true
					}
					if flags&0x000100 != 0 {
						if off+4 > len(p) {
							break
						}
						sDur = binary.BigEndian.Uint32(p[off : off+4])
						off += 4
					}
					if flags&0x000200 != 0 {
						off += 4
					}
					if flags&0x000400 != 0 {
						if off+4 > len(p) {
							break
						}
						sFlags = binary.BigEndian.Uint32(p[off : off+4])
						hasSFlags = true
						off += 4
					}
					if flags&0x000800 != 0 {
						off += 4
					}
					totalDur += sDur
					if i == 0 && hasSFlags {
						sample0Flags = sFlags
						hasSample0Flags = true
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		isVideo := init != nil && init.VideoTrackID != 0 && trackID == init.VideoTrackID
		if !hasSample0Flags && hasDefaultFlags {
			sample0Flags = defaultFlags
			hasSample0Flags = true
		}
		isKeyframe := isVideo && hasSample0Flags && (sample0Flags&0x00010000) == 0
		if totalDur == 0 {
			totalDur = defaultDur
		}

		frag := Fragment{
			Moof:       moofRaw,
			TrackID:    trackID,
			IsVideo:    isVideo,
			IsKeyframe: isKeyframe,
			DTS:        dts,
			Duration:   totalDur,
		}
		if !found || frag.IsVideo {
			best = frag
			found = true
		}
		return nil
	})
	if err != nil {
		return Fragment{}, fmt.Errorf("parse moof: %w", err)
	}
	if !found {
		return Fragment{}, errors.New("moof contains no traf box")
	}
	return best, nil
}

// ClipRebaser rewrites mfhd sequence numbers and tfdt base decode timestamps
// so every extracted clip begins cleanly at sequence 1 and DTS 0 per track.
type ClipRebaser struct {
	seq     uint32
	baseDTS map[uint32]uint64
}

// NewClipRebaser creates a fresh ClipRebaser for a new output clip.
func NewClipRebaser() *ClipRebaser {
	return &ClipRebaser{
		baseDTS: make(map[uint32]uint64),
	}
}

// RebaseMoof returns a cloned moof box with rebased sequence_number and tfdt values.
// The input slice is never mutated.
func (r *ClipRebaser) RebaseMoof(moofRaw []byte) ([]byte, error) {
	moofHdr, err := boxHeaderLen(moofRaw)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(moofRaw))
	copy(out, moofRaw)

	r.seq++

	err = iterBoxes(out, moofHdr, len(out), func(cb childBox) error {
		switch cb.typ {
		case "mfhd":
			if cb.end-cb.payload >= 8 {
				binary.BigEndian.PutUint32(out[cb.payload+4:cb.payload+8], r.seq)
			}
		case "traf":
			var trackID uint32
			tfdtPayload := -1
			tfdtEnd := -1

			if err := iterBoxes(out, cb.payload, cb.end, func(tc childBox) error {
				switch tc.typ {
				case "tfhd":
					if tc.end-tc.payload >= 8 {
						trackID = binary.BigEndian.Uint32(out[tc.payload+4 : tc.payload+8])
					}
				case "tfdt":
					tfdtPayload = tc.payload
					tfdtEnd = tc.end
				}
				return nil
			}); err != nil {
				return err
			}

			if tfdtPayload >= 0 && tfdtEnd-tfdtPayload >= 8 {
				ver := out[tfdtPayload]
				var orig uint64
				if ver == 0 {
					orig = uint64(binary.BigEndian.Uint32(out[tfdtPayload+4 : tfdtPayload+8]))
				} else if tfdtEnd-tfdtPayload >= 12 {
					orig = binary.BigEndian.Uint64(out[tfdtPayload+4 : tfdtPayload+12])
				}

				base, ok := r.baseDTS[trackID]
				if !ok {
					base = orig
					r.baseDTS[trackID] = base
				}
				var rebased uint64
				if orig > base {
					rebased = orig - base
				}

				if ver == 0 {
					binary.BigEndian.PutUint32(out[tfdtPayload+4:tfdtPayload+8], uint32(rebased))
				} else if tfdtEnd-tfdtPayload >= 12 {
					binary.BigEndian.PutUint64(out[tfdtPayload+4:tfdtPayload+12], rebased)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
