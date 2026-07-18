package protocol

import "testing"

func TestFrameCompatibilityRequiresExplicitProtocolIdentity(t *testing.T) {
	if FrameCompatible(Frame{}) {
		t.Fatal("frame without a protocol identity was accepted")
	}
	if FrameCompatible(Frame{ProtocolVersion: Version - 1}) {
		t.Fatal("legacy protocol frame was accepted")
	}
	if !FrameCompatible(Frame{ProtocolMin: MinVersion, ProtocolMax: MaxVersion}) {
		t.Fatal("current protocol range was rejected")
	}
}
