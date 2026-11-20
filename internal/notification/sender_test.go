package notification

import (
	"context"
	"errors"
	"log/slog"
	"testing"
)

type mockChannel struct {
	name     Channel
	sendErr  error
	sentMsgs []Message
}

func (m *mockChannel) Name() Channel {
	return m.name
}

func (m *mockChannel) Send(ctx context.Context, msg Message) error {
	m.sentMsgs = append(m.sentMsgs, msg)
	return m.sendErr
}

func TestSender_Send_NoChannels(t *testing.T) {
	s := NewSender(slog.Default())
	msg := Message{Title: "test"}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Errorf("expected no error when no channels configured, got %v", err)
	}
}

func TestSender_Send_AllChannels(t *testing.T) {
	ch1 := &mockChannel{name: ChannelWebPush}
	ch2 := &mockChannel{name: ChannelTelegram}
	s := NewSender(slog.Default(), ch1, ch2)

	msg := Message{Title: "test", Text: "body"}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if len(ch1.sentMsgs) != 1 {
		t.Errorf("channel 1 expected 1 message, got %d", len(ch1.sentMsgs))
	}
	if len(ch2.sentMsgs) != 1 {
		t.Errorf("channel 2 expected 1 message, got %d", len(ch2.sentMsgs))
	}
}

func TestSender_Send_TargetChannel(t *testing.T) {
	ch1 := &mockChannel{name: ChannelWebPush}
	ch2 := &mockChannel{name: ChannelTelegram}
	s := NewSender(slog.Default(), ch1, ch2)

	msg := Message{Channel: ChannelTelegram, Title: "test"}
	if err := s.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send failed: %v", err)
	}

	if len(ch1.sentMsgs) != 0 {
		t.Errorf("webpush should not receive message, got %d", len(ch1.sentMsgs))
	}
	if len(ch2.sentMsgs) != 1 {
		t.Errorf("telegram expected 1 message, got %d", len(ch2.sentMsgs))
	}
}

func TestSender_Send_ChannelNotFound(t *testing.T) {
	ch1 := &mockChannel{name: ChannelWebPush}
	s := NewSender(slog.Default(), ch1)

	msg := Message{Channel: ChannelTelegram, Title: "test"}
	err := s.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error when target channel not found")
	}
	if len(ch1.sentMsgs) != 0 {
		t.Errorf("no message should be sent, got %d", len(ch1.sentMsgs))
	}
}

func TestSender_Send_ChannelError(t *testing.T) {
	expectedErr := errors.New("channel failed")
	ch1 := &mockChannel{name: ChannelTelegram, sendErr: expectedErr}
	s := NewSender(slog.Default(), ch1)

	msg := Message{Title: "test"}
	err := s.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error to be returned")
	}
	if len(ch1.sentMsgs) != 1 {
		t.Errorf("message should still be attempted, got %d", len(ch1.sentMsgs))
	}
}

func TestSender_Send_PartialFailure(t *testing.T) {
	ch1 := &mockChannel{name: ChannelWebPush}
	ch2 := &mockChannel{name: ChannelTelegram, sendErr: errors.New("telegram failed")}
	s := NewSender(slog.Default(), ch1, ch2)

	msg := Message{Title: "test"}
	err := s.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error when one channel fails")
	}

	if len(ch1.sentMsgs) != 1 {
		t.Errorf("webpush should succeed, got %d messages", len(ch1.sentMsgs))
	}
	if len(ch2.sentMsgs) != 1 {
		t.Errorf("telegram should be attempted, got %d messages", len(ch2.sentMsgs))
	}
}
