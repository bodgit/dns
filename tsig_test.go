package dns

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func newTsig(algo string) *Msg {
	m := new(Msg)
	m.SetQuestion("example.org.", TypeA)
	m.SetTsig("example.", algo, 300, time.Now().Unix())
	return m
}

func TestTsig(t *testing.T) {
	m := newTsig(HmacSHA256)
	buf, _, err := TsigGenerate(m, "pRZgBrBvI4NAHZYhxmhs/Q==", "", false)
	if err != nil {
		t.Fatal(err)
	}
	err = TsigVerify(buf, "pRZgBrBvI4NAHZYhxmhs/Q==", "", false)
	if err != nil {
		t.Fatal(err)
	}

	// TSIG accounts for ID substitution. This means if the message ID is
	// changed by a forwarder, we should still be able to verify the TSIG.
	m = newTsig(HmacSHA256)
	buf, _, err = TsigGenerate(m, "pRZgBrBvI4NAHZYhxmhs/Q==", "", false)
	if err != nil {
		t.Fatal(err)
	}

	binary.BigEndian.PutUint16(buf[0:2], 42)
	err = TsigVerify(buf, "pRZgBrBvI4NAHZYhxmhs/Q==", "", false)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTsigCase(t *testing.T) {
	m := newTsig(strings.ToUpper(HmacSHA256))
	buf, _, err := TsigGenerate(m, "pRZgBrBvI4NAHZYhxmhs/Q==", "", false)
	if err != nil {
		t.Fatal(err)
	}
	err = TsigVerify(buf, "pRZgBrBvI4NAHZYhxmhs/Q==", "", false)
	if err != nil {
		t.Fatal(err)
	}
}

const (
	// A template wire-format DNS message (in hex form) containing a TSIG RR.
	// Its time signed field will be filled by tests.
	wireMsg = "c60028000001000000010001076578616d706c6503636f6d00000600010161c00c0001000100000e100004c0000201077465" +
		"73746b65790000fa00ff00000000003d0b686d61632d73686132353600" +
		"%012x" + // placeholder for the "time signed" field
		"012c00208cf23e0081d915478a182edcea7ff48ad102948e6c7ef8e887536957d1fa5616c60000000000"
	// A secret (in base64 format) with which the TSIG in wireMsg will be validated
	testSecret = "NoTCJU+DMqFWywaPyxSijrDEA/eC3nK0xi3AMEZuPVk="
	// the 'time signed' field value that would make the TSIG RR valid with testSecret
	timeSigned uint64 = 1594855491
)

func TestTsigErrors(t *testing.T) {
	// Helper shortcut to build wire-format test message.
	// TsigVerify can modify the slice, so we need to create a new one for each test case below.
	buildMsgData := func(tm uint64) []byte {
		msgData, err := hex.DecodeString(fmt.Sprintf(wireMsg, tm))
		if err != nil {
			t.Fatal(err)
		}
		return msgData
	}

	// the signature is valid but 'time signed' is too far from the "current time".
	if err := tsigVerify(buildMsgData(timeSigned), "", false, timeSigned+301, tsigHMACProvider(testSecret)); err != ErrTime {
		t.Fatalf("expected an error '%v' but got '%v'", ErrTime, err)
	}
	if err := tsigVerify(buildMsgData(timeSigned), "", false, timeSigned-301, tsigHMACProvider(testSecret)); err != ErrTime {
		t.Fatalf("expected an error '%v' but got '%v'", ErrTime, err)
	}

	// the signature is invalid and 'time signed' is too far.
	// the signature should be checked first, so we should see ErrSig.
	if err := tsigVerify(buildMsgData(timeSigned+301), "", false, timeSigned, tsigHMACProvider(testSecret)); err != ErrSig {
		t.Fatalf("expected an error '%v' but got '%v'", ErrSig, err)
	}

	// tweak the algorithm name in the wire data, resulting in the "unknown algorithm" error.
	msgData := buildMsgData(timeSigned)
	copy(msgData[67:], "bogus")
	if err := tsigVerify(msgData, "", false, timeSigned, tsigHMACProvider(testSecret)); err != ErrKeyAlg {
		t.Fatalf("expected an error '%v' but got '%v'", ErrKeyAlg, err)
	}

	// call TsigVerify with a message that doesn't contain a TSIG
	msgData, tsig, err := stripTsig(buildMsgData(timeSigned))
	if err != nil {
		t.Fatal(err)
	}
	if err := tsigVerify(msgData, "", false, timeSigned, tsigHMACProvider(testSecret)); err != ErrNoSig {
		t.Fatalf("expected an error '%v' but got '%v'", ErrNoSig, err)
	}

	// replace the test TSIG with a bogus one with large "other data", which would cause overflow in TsigVerify.
	// The overflow should be caught without disruption.
	tsig.OtherData = strings.Repeat("00", 4096)
	tsig.OtherLen = uint16(len(tsig.OtherData) / 2)
	msg := new(Msg)
	if err = msg.Unpack(msgData); err != nil {
		t.Fatal(err)
	}
	msg.Extra = append(msg.Extra, tsig)
	if msgData, err = msg.Pack(); err != nil {
		t.Fatal(err)
	}
	err = tsigVerify(msgData, "", false, timeSigned, tsigHMACProvider(testSecret))
	if err == nil || !strings.Contains(err.Error(), "overflow") {
		t.Errorf("expected error to contain %q, but got %v", "overflow", err)
	}
}

// This test exercises some more corner cases for TsigGenerate.
func TestTsigGenerate(t *testing.T) {
	// This is a template TSIG to be used for signing.
	tsig := TSIG{
		Hdr:        RR_Header{Name: "testkey.", Rrtype: TypeTSIG, Class: ClassANY, Ttl: 0},
		Algorithm:  HmacSHA256,
		TimeSigned: timeSigned,
		Fudge:      300,
		OrigId:     42,
		Error:      RcodeBadTime, // use a non-0 value to make sure it's indeed used
	}

	tests := []struct {
		desc        string // test description
		requestMAC  string // request MAC to be passed to TsigGenerate (arbitrary choice)
		otherData   string // other data specified in the TSIG (arbitrary choice)
		expectedMAC string // pre-computed expected (correct) MAC in hex form
	}{
		{"with request MAC", "3684c225", "",
			"c110e3f62694755c10761dc8717462431ee34340b7c9d1eee09449150757c5b1"},
		{"no request MAC", "", "",
			"385449a425c6d52b9bf2c65c0726eefa0ad8084cdaf488f24547e686605b9610"},
		{"with other data", "3684c225", "666f6f",
			"15b91571ca80b3b410a77e2b44f8cc4f35ace22b26020138439dd94803e23b5d"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.desc, func(t *testing.T) {
			// Build TSIG for signing from the template
			testTSIG := tsig
			testTSIG.OtherLen = uint16(len(tc.otherData) / 2)
			testTSIG.OtherData = tc.otherData
			req := &Msg{
				MsgHdr:   MsgHdr{Opcode: OpcodeUpdate},
				Question: []Question{Question{Name: "example.com.", Qtype: TypeSOA, Qclass: ClassINET}},
				Extra:    []RR{&testTSIG},
			}

			// Call generate, and check the returned MAC against the expected value
			msgData, mac, err := TsigGenerate(req, testSecret, tc.requestMAC, false)
			if err != nil {
				t.Error(err)
			}
			if mac != tc.expectedMAC {
				t.Fatalf("MAC doesn't match: expected '%s', but got '%s'", tc.expectedMAC, mac)
			}

			// Retrieve the TSIG to be sent out, confirm the MAC in it
			_, outTSIG, err := stripTsig(msgData)
			if err != nil {
				t.Error(err)
			}
			if outTSIG.MAC != tc.expectedMAC {
				t.Fatalf("MAC doesn't match: expected '%s', but got '%s'", tc.expectedMAC, outTSIG.MAC)
			}
			// Confirm other fields of MAC.
			// RDLENGTH should be valid as stripTsig succeeded, so we exclude it from comparison
			outTSIG.MACSize = 0
			outTSIG.MAC = ""
			testTSIG.Hdr.Rdlength = outTSIG.Hdr.Rdlength
			if *outTSIG != testTSIG {
				t.Fatalf("TSIG RR doesn't match: expected '%v', but got '%v'", *outTSIG, testTSIG)
			}
		})
	}
}

func TestTSIGHMAC224And384(t *testing.T) {
	tests := []struct {
		algorithm   string // TSIG algorithm, also used as test description
		secret      string // (arbitrarily chosen) secret suitable for the algorithm in base64 format
		expectedMAC string // pre-computed expected (correct) MAC in hex form
	}{
		{HmacSHA224, "hVEkQuAqnTmBuRrT9KF1Udr91gOMGWPw9LaTtw==",
			"d6daf9ea189e48bc38f9aed63d6cc4140cdfa38a7a333ee2eefdbd31",
		},
		{HmacSHA384, "Qjer2TL2lAdpq9w6Gjs98/ClCQx/L3vtgVHCmrZ8l/oKEPjqUUMFO18gMCRwd5H4",
			"89a48936d29187870c325cbdba5ad71609bd038d0459d6010c844d659c570e881d3650e4fe7310be53ebe5178d0d1001",
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.algorithm, func(t *testing.T) {
			// Build a DNS message with TSIG for the test scenario
			tsig := TSIG{
				Hdr:        RR_Header{Name: "testkey.", Rrtype: TypeTSIG, Class: ClassANY, Ttl: 0},
				Algorithm:  tc.algorithm,
				TimeSigned: timeSigned,
				Fudge:      300,
				OrigId:     42,
			}
			req := &Msg{
				MsgHdr:   MsgHdr{Opcode: OpcodeUpdate},
				Question: []Question{Question{Name: "example.com.", Qtype: TypeSOA, Qclass: ClassINET}},
				Extra:    []RR{&tsig},
			}

			// Confirm both Generate and Verify recognize the algorithm and handle it correctly
			msgData, mac, err := TsigGenerate(req, tc.secret, "", false)
			if err != nil {
				t.Error(err)
			}
			if mac != tc.expectedMAC {
				t.Fatalf("MAC doesn't match: expected '%s' but got '%s'", tc.expectedMAC, mac)
			}
			if err = tsigVerify(msgData, "", false, timeSigned, tsigHMACProvider(tc.secret)); err != nil {
				t.Error(err)
			}
		})
	}
}

const (
	testGoodKeyName = "goodkey."
)

var (
	testErrBadKey = errors.New("this is an intentional error")
	testGoodMAC   = []byte{0, 1, 2, 3}
)

// testProvider always generates the same MAC and only accepts the one signature
type testProvider struct {
	GenerateAllKeys bool
}

func (provider *testProvider) Generate(_ []byte, t *TSIG) ([]byte, error) {
	switch {
	case t.Hdr.Name == testGoodKeyName || provider.GenerateAllKeys:
		return testGoodMAC, nil
	default:
		return nil, testErrBadKey
	}
}

func (*testProvider) Verify(_ []byte, t *TSIG) error {
	switch t.Hdr.Name {
	case testGoodKeyName:
		return nil
	default:
		return testErrBadKey
	}
}

func TestTsigGenerateProvider(t *testing.T) {
	tables := map[string]struct {
		keyname string
		mac     []byte
		err     error
	}{
		"ok": {
			testGoodKeyName,
			testGoodMAC,
			nil,
		},
		"bad": {
			"badkey.",
			nil,
			testErrBadKey,
		},
	}

	for name, table := range tables {
		t.Run(name, func(t *testing.T) {
			tsig := TSIG{
				Hdr:        RR_Header{Name: table.keyname, Rrtype: TypeTSIG, Class: ClassANY, Ttl: 0},
				Algorithm:  HmacSHA1,
				TimeSigned: timeSigned,
				Fudge:      300,
				OrigId:     42,
			}
			req := &Msg{
				MsgHdr:   MsgHdr{Opcode: OpcodeUpdate},
				Question: []Question{Question{Name: "example.com.", Qtype: TypeSOA, Qclass: ClassINET}},
				Extra:    []RR{&tsig},
			}

			_, mac, err := tsigGenerateProvider(req, "", false, new(testProvider))
			if err != table.err {
				t.Fatalf("error doesn't match: expected '%s' but got '%s'", table.err, err)
			}
			expectedMAC := hex.EncodeToString(table.mac)
			if mac != expectedMAC {
				t.Fatalf("MAC doesn't match: expected '%s' but got '%s'", table.mac, expectedMAC)
			}
		})
	}
}

func TestTsigVerifyProvider(t *testing.T) {
	tables := map[string]struct {
		keyname string
		err     error
	}{
		"ok": {
			testGoodKeyName,
			nil,
		},
		"bad": {
			"badkey.",
			testErrBadKey,
		},
	}

	for name, table := range tables {
		t.Run(name, func(t *testing.T) {
			tsig := TSIG{
				Hdr:        RR_Header{Name: table.keyname, Rrtype: TypeTSIG, Class: ClassANY, Ttl: 0},
				Algorithm:  HmacSHA1,
				TimeSigned: timeSigned,
				Fudge:      300,
				OrigId:     42,
			}
			req := &Msg{
				MsgHdr:   MsgHdr{Opcode: OpcodeUpdate},
				Question: []Question{Question{Name: "example.com.", Qtype: TypeSOA, Qclass: ClassINET}},
				Extra:    []RR{&tsig},
			}

			provider := &testProvider{true}
			msgData, _, err := tsigGenerateProvider(req, "", false, provider)
			if err != nil {
				t.Error(err)
			}
			if err = tsigVerify(msgData, "", false, timeSigned, provider); err != table.err {
				t.Fatalf("error doesn't match: expected '%s' but got '%s'", table.err, err)
			}
		})
	}
}
