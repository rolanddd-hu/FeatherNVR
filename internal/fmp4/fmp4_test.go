package fmp4

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

func makeBox(typ string, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(len(b)))
	copy(b[4:8], typ)
	copy(b[8:], payload)
	return b
}

func makeFullBox(typ string, version byte, flags uint32, body []byte) []byte {
	p := make([]byte, 4+len(body))
	p[0] = version
	p[1] = byte(flags >> 16)
	p[2] = byte(flags >> 8)
	p[3] = byte(flags)
	copy(p[4:], body)
	return makeBox(typ, p)
}

func buildTestInit() (ftyp []byte, moov []byte) {
	ftyp = makeBox("ftyp", []byte("iso5\x00\x00\x02\x00iso5iso6mp41"))

	buildTrak := func(trackID uint32, handler string, timescale uint32) []byte {
		tkhdBody := make([]byte, 80)
		binary.BigEndian.PutUint32(tkhdBody[8:12], trackID)
		if handler == "vide" {
			// tkhdBody starts after 4-byte FullBox header -> tkhd width at offset 76-4=72, height at 80-4=76.
			binary.BigEndian.PutUint16(tkhdBody[72:74], 1280)
			binary.BigEndian.PutUint16(tkhdBody[76:78], 720)
		}
		tkhd := makeFullBox("tkhd", 0, 3, tkhdBody)

		mdhdBody := make([]byte, 20)
		binary.BigEndian.PutUint32(mdhdBody[8:12], timescale)
		mdhd := makeFullBox("mdhd", 0, 0, mdhdBody)

		hdlrBody := make([]byte, 12)
		copy(hdlrBody[4:8], handler)
		hdlr := makeFullBox("hdlr", 0, 0, hdlrBody)

		mdiaChildren := append(mdhd, hdlr...)
		if handler == "vide" {
			sampleEntry := make([]byte, 78)
			binary.BigEndian.PutUint16(sampleEntry[24:26], 1920)
			binary.BigEndian.PutUint16(sampleEntry[26:28], 1080)
			avc1 := makeBox("avc1", sampleEntry)
			stsdBody := make([]byte, 4+len(avc1))
			binary.BigEndian.PutUint32(stsdBody[0:4], 1)
			copy(stsdBody[4:], avc1)
			stsd := makeFullBox("stsd", 0, 0, stsdBody)
			stbl := makeBox("stbl", stsd)
			minf := makeBox("minf", stbl)
			mdiaChildren = append(mdiaChildren, minf...)
		}

		mdia := makeBox("mdia", mdiaChildren)
		return makeBox("trak", append(tkhd, mdia...))
	}

	trakV := buildTrak(1, "vide", 90000)
	trakA := buildTrak(2, "soun", 48000)
	moov = makeBox("moov", append(trakV, trakA...))
	return ftyp, moov
}

func buildTestMoof(seq uint32, trackID uint32, duration uint32, flags uint32, dts uint64, v1 bool) []byte {
	mfhdBody := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdBody, seq)
	mfhd := makeFullBox("mfhd", 0, 0, mfhdBody)

	tfhdBody := make([]byte, 16)
	binary.BigEndian.PutUint32(tfhdBody[0:4], trackID)
	binary.BigEndian.PutUint32(tfhdBody[4:8], duration)
	binary.BigEndian.PutUint32(tfhdBody[8:12], 512)
	binary.BigEndian.PutUint32(tfhdBody[12:16], flags)
	tfhd := makeFullBox("tfhd", 0, 0x020038, tfhdBody)

	var tfdt []byte
	if v1 {
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, dts)
		tfdt = makeFullBox("tfdt", 1, 0, b)
	} else {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, uint32(dts))
		tfdt = makeFullBox("tfdt", 0, 0, b)
	}

	trunBody := make([]byte, 8)
	binary.BigEndian.PutUint32(trunBody[0:4], 1)
	binary.BigEndian.PutUint32(trunBody[4:8], 64)
	trun := makeFullBox("trun", 0, 0x000001, trunBody)

	trafPayload := append(tfhd, tfdt...)
	trafPayload = append(trafPayload, trun...)
	traf := makeBox("traf", trafPayload)

	return makeBox("moof", append(mfhd, traf...))
}

func TestReadBoxParseInitAndMoof(t *testing.T) {
	ftyp, moov := buildTestInit()
	moofKey := buildTestMoof(10, 1, 3000, 0x02000000, 900000, true)
	mdatKey := makeBox("mdat", []byte{1, 2, 3, 4})
	moofP := buildTestMoof(11, 1, 3000, 0x01010000, 903000, false)
	mdatP := makeBox("mdat", []byte{5, 6, 7, 8})
	moofAudio := buildTestMoof(12, 2, 1024, 0x02000000, 480000, true)
	mdatAudio := makeBox("mdat", []byte{9, 10})

	var stream bytes.Buffer
	stream.Write(ftyp)
	stream.Write(moov)
	stream.Write(moofKey)
	stream.Write(mdatKey)
	stream.Write(moofP)
	stream.Write(mdatP)
	stream.Write(moofAudio)
	stream.Write(mdatAudio)

	b1, err := ReadBox(&stream)
	if err != nil || b1.Type != "ftyp" {
		t.Fatalf("read ftyp: %v %+v", err, b1.Type)
	}
	b2, err := ReadBox(&stream)
	if err != nil || b2.Type != "moov" {
		t.Fatalf("read moov: %v %+v", err, b2.Type)
	}

	init, err := ParseInit(b1.Raw, b2.Raw)
	if err != nil {
		t.Fatalf("ParseInit failed: %v", err)
	}
	if init.VideoTrackID != 1 {
		t.Fatalf("expected VideoTrackID=1, got %d", init.VideoTrackID)
	}
	if init.Width != 1920 || init.Height != 1080 || init.Codec != "H.264" {
		t.Fatalf("expected 1920x1080 H.264, got %dx%d %q", init.Width, init.Height, init.Codec)
	}
	if init.Timescales[1] != 90000 || init.Timescales[2] != 48000 {
		t.Fatalf("unexpected timescales: %+v", init.Timescales)
	}

	b3, _ := ReadBox(&stream)
	fKey, err := ParseMoof(b3.Raw, init)
	if err != nil {
		t.Fatalf("ParseMoof keyframe failed: %v", err)
	}
	if !fKey.IsVideo || !fKey.IsKeyframe || fKey.DTS != 900000 || fKey.Duration != 3000 {
		t.Fatalf("unexpected keyframe parsed: %+v", fKey)
	}
	_, _ = ReadBox(&stream) // mdatKey

	b5, _ := ReadBox(&stream)
	fP, err := ParseMoof(b5.Raw, init)
	if err != nil {
		t.Fatalf("ParseMoof p-frame failed: %v", err)
	}
	if !fP.IsVideo || fP.IsKeyframe || fP.DTS != 903000 {
		t.Fatalf("unexpected p-frame parsed: %+v", fP)
	}
	_, _ = ReadBox(&stream) // mdatP

	b7, _ := ReadBox(&stream)
	fA, err := ParseMoof(b7.Raw, init)
	if err != nil {
		t.Fatalf("ParseMoof audio failed: %v", err)
	}
	if fA.IsVideo || fA.IsKeyframe || fA.TrackID != 2 || fA.DTS != 480000 {
		t.Fatalf("unexpected audio parsed: %+v", fA)
	}
}

func TestClipRebaserDoesNotMutateInput(t *testing.T) {
	ftyp, moov := buildTestInit()
	init, err := ParseInit(ftyp, moov)
	if err != nil {
		t.Fatalf("ParseInit: %v", err)
	}

	origKey := buildTestMoof(42, 1, 3000, 0x02000000, 900000, true)
	origKeyCopy := append([]byte(nil), origKey...)
	origP := buildTestMoof(43, 1, 3000, 0x01010000, 903000, true)
	origA := buildTestMoof(44, 2, 1024, 0x02000000, 480000, false)
	origA2 := buildTestMoof(45, 2, 1024, 0x02000000, 481024, false)

	rebaser := NewClipRebaser()
	r1, err := rebaser.RebaseMoof(origKey)
	if err != nil {
		t.Fatalf("RebaseMoof 1: %v", err)
	}
	if !bytes.Equal(origKey, origKeyCopy) {
		t.Fatalf("RebaseMoof mutated input slice")
	}

	r2, _ := rebaser.RebaseMoof(origP)
	r3, _ := rebaser.RebaseMoof(origA)
	r4, _ := rebaser.RebaseMoof(origA2)

	f1, _ := ParseMoof(r1, init)
	f2, _ := ParseMoof(r2, init)
	f3, _ := ParseMoof(r3, init)
	f4, _ := ParseMoof(r4, init)

	if f1.DTS != 0 || f2.DTS != 3000 {
		t.Fatalf("unexpected video rebased DTS: f1=%d f2=%d", f1.DTS, f2.DTS)
	}
	if f3.DTS != 0 || f4.DTS != 1024 {
		t.Fatalf("unexpected audio rebased DTS: f3=%d f4=%d", f3.DTS, f4.DTS)
	}
}

func TestGOPRingBufferDurationAndBytes(t *testing.T) {
	t0 := time.Unix(1000, 0)
	rb := NewGOPRingBuffer(5*time.Second, 1024)

	// Leading P-frame before first keyframe must be ignored.
	rb.Push(Fragment{Moof: make([]byte, 50), Mdat: make([]byte, 50), IsVideo: true, IsKeyframe: false, ReceivedAt: t0})
	if len(rb.Snapshot()) != 0 {
		t.Fatalf("expected leading non-keyframe to be dropped")
	}

	// Push GOPs at 0s, 2s, 4s, 6s, 8s (each 200 bytes: 100 key + 100 P).
	for i := 0; i < 5; i++ {
		ts := t0.Add(time.Duration(i*2) * time.Second)
		rb.Push(Fragment{Moof: make([]byte, 50), Mdat: make([]byte, 50), DTS: uint64(i * 2), IsVideo: true, IsKeyframe: true, ReceivedAt: ts})
		rb.Push(Fragment{Moof: make([]byte, 50), Mdat: make([]byte, 50), DTS: uint64(i*2 + 1), IsVideo: true, IsKeyframe: false, ReceivedAt: ts.Add(500 * time.Millisecond)})
	}

	snap := rb.Snapshot()
	if len(snap) == 0 || !snap[0].IsKeyframe {
		t.Fatalf("snapshot must start on keyframe")
	}
	// At t=8s, GOP1 starts at t=2s (8 - 2 = 6s >= 5s), so GOP0 (t=0) evicted and GOP1 (t=2) retained.
	if snap[0].DTS != 2 {
		t.Fatalf("expected oldest retained GOP DTS=2, got %d", snap[0].DTS)
	}

	// Byte limit eviction test: maxBytes=250 allows at most 1 GOP (200 bytes) + keyframe.
	rbSmall := NewGOPRingBuffer(60*time.Second, 250)
	for i := 0; i < 3; i++ {
		ts := t0.Add(time.Duration(i) * time.Second)
		rbSmall.Push(Fragment{Moof: make([]byte, 100), Mdat: make([]byte, 100), DTS: uint64(i), IsVideo: true, IsKeyframe: true, ReceivedAt: ts})
	}
	snapSmall := rbSmall.Snapshot()
	if len(snapSmall) != 1 || snapSmall[0].DTS != 2 {
		t.Fatalf("expected byte-bounded eviction to retain newest GOP DTS=2, got %+v", snapSmall)
	}
}

func buildTestInitWithAudioSample(audioFourCC string, audioTimescale uint32) (ftyp []byte, moov []byte) {
	ftyp = makeBox("ftyp", []byte("iso5\x00\x00\x02\x00iso5iso6mp41"))

	buildTrak := func(trackID uint32, handler string, timescale uint32, sampleFourCC string) []byte {
		tkhdBody := make([]byte, 80)
		binary.BigEndian.PutUint32(tkhdBody[8:12], trackID)
		tkhd := makeFullBox("tkhd", 0, 3, tkhdBody)

		mdhdBody := make([]byte, 20)
		binary.BigEndian.PutUint32(mdhdBody[8:12], timescale)
		mdhd := makeFullBox("mdhd", 0, 0, mdhdBody)

		hdlrBody := make([]byte, 12)
		copy(hdlrBody[4:8], handler)
		hdlr := makeFullBox("hdlr", 0, 0, hdlrBody)

		var sampleEntry []byte
		if handler == "vide" {
			payload := make([]byte, 78)
			binary.BigEndian.PutUint16(payload[24:26], 1920)
			binary.BigEndian.PutUint16(payload[26:28], 1080)
			sampleEntry = makeBox(sampleFourCC, payload)
		} else {
			payload := make([]byte, 28)
			binary.BigEndian.PutUint16(payload[16:18], 1)
			binary.BigEndian.PutUint16(payload[18:20], 16)
			binary.BigEndian.PutUint32(payload[24:28], timescale<<16)
			sampleEntry = makeBox(sampleFourCC, payload)
		}

		stsdBody := make([]byte, 4+len(sampleEntry))
		binary.BigEndian.PutUint32(stsdBody[0:4], 1)
		copy(stsdBody[4:], sampleEntry)
		stsd := makeFullBox("stsd", 0, 0, stsdBody)
		minf := makeBox("minf", makeBox("stbl", stsd))
		mdia := makeBox("mdia", append(append(mdhd, hdlr...), minf...))
		return makeBox("trak", append(tkhd, mdia...))
	}

	trakV := buildTrak(1, "vide", 90000, "avc1")
	trakA := buildTrak(2, "soun", audioTimescale, audioFourCC)
	moov = makeBox("moov", append(trakV, trakA...))
	return ftyp, moov
}

func buildMultiTrafMoof(seq uint32, videoDTS, audioDTS uint64) []byte {
	mfhdBody := make([]byte, 4)
	binary.BigEndian.PutUint32(mfhdBody, seq)
	mfhd := makeFullBox("mfhd", 0, 0, mfhdBody)

	makeTraf := func(trackID uint32, dur uint32, flags uint32, dts uint64) []byte {
		tfhdBody := make([]byte, 16)
		binary.BigEndian.PutUint32(tfhdBody[0:4], trackID)
		binary.BigEndian.PutUint32(tfhdBody[4:8], dur)
		binary.BigEndian.PutUint32(tfhdBody[8:12], 512)
		binary.BigEndian.PutUint32(tfhdBody[12:16], flags)
		tfhd := makeFullBox("tfhd", 0, 0x020038, tfhdBody)

		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, dts)
		tfdt := makeFullBox("tfdt", 1, 0, b)

		trunBody := make([]byte, 8)
		binary.BigEndian.PutUint32(trunBody[0:4], 1)
		binary.BigEndian.PutUint32(trunBody[4:8], 64)
		trun := makeFullBox("trun", 0, 0x000001, trunBody)
		return makeBox("traf", append(append(tfhd, tfdt...), trun...))
	}

	trafV := makeTraf(1, 3000, 0x02000000, videoDTS)
	trafA := makeTraf(2, 1024, 0x02000000, audioDTS)
	return makeBox("moof", append(append(mfhd, trafV...), trafA...))
}

func TestParseInitAndMoofAudioMetadata(t *testing.T) {
	ftypFlac, moovFlac := buildTestInitWithAudioSample("fLaC", 8000)
	initFlac, err := ParseInit(ftypFlac, moovFlac)
	if err != nil {
		t.Fatalf("ParseInit fLaC failed: %v", err)
	}
	if initFlac.AudioTrackID != 2 || initFlac.AudioSampleType != "fLaC" || initFlac.AudioTimescale() != 8000 {
		t.Fatalf("unexpected fLaC audio init metadata: id=%d sample=%q timescale=%d",
			initFlac.AudioTrackID, initFlac.AudioSampleType, initFlac.AudioTimescale())
	}

	ftypAAC, moovAAC := buildTestInitWithAudioSample("mp4a", 16000)
	initAAC, err := ParseInit(ftypAAC, moovAAC)
	if err != nil {
		t.Fatalf("ParseInit mp4a failed: %v", err)
	}
	if initAAC.AudioTrackID != 2 || initAAC.AudioSampleType != "mp4a" || initAAC.AudioTimescale() != 16000 {
		t.Fatalf("unexpected mp4a audio init metadata: id=%d sample=%q timescale=%d",
			initAAC.AudioTrackID, initAAC.AudioSampleType, initAAC.AudioTimescale())
	}

	var nilInit *InitInfo
	if nilInit.AudioTimescale() != 0 {
		t.Fatalf("expected 0 AudioTimescale on nil InitInfo")
	}

	// Video-only moof -> IsVideo=true, IsAudio=false.
	moofV := buildTestMoof(1, 1, 3000, 0x02000000, 90000, true)
	fragV, err := ParseMoof(moofV, initFlac)
	if err != nil {
		t.Fatalf("ParseMoof video-only failed: %v", err)
	}
	if !fragV.IsVideo || fragV.IsAudio || !fragV.IsKeyframe {
		t.Fatalf("expected video-only keyframe fragment, got %+v", fragV)
	}

	// Audio-only moof -> IsVideo=false, IsAudio=true.
	moofA := buildTestMoof(2, 2, 1024, 0x02000000, 8000, true)
	fragA, err := ParseMoof(moofA, initFlac)
	if err != nil {
		t.Fatalf("ParseMoof audio-only failed: %v", err)
	}
	if fragA.IsVideo || !fragA.IsAudio || fragA.IsKeyframe {
		t.Fatalf("expected audio-only fragment, got %+v", fragA)
	}

	// Combined video+audio multi-traf moof -> IsVideo=true and IsAudio=true.
	moofBoth := buildMultiTrafMoof(3, 93000, 9024)
	fragBoth, err := ParseMoof(moofBoth, initFlac)
	if err != nil {
		t.Fatalf("ParseMoof multi-traf failed: %v", err)
	}
	if !fragBoth.IsVideo || !fragBoth.IsAudio || !fragBoth.IsKeyframe || fragBoth.TrackID != 1 {
		t.Fatalf("expected combined video+audio keyframe fragment, got %+v", fragBoth)
	}
}
