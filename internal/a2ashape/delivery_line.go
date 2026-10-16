package a2ashape

import (
	"time"
)

// DeliveryState is the delivery state of a task as its projection states it.
type DeliveryState struct {
	Delivery, Detail, Wait string
	Ordered                bool
	// Since is when the message was queued; EndsAt when the task gives up on
	// it (pending only). Zero when the projection does not say.
	Since, EndsAt time.Time
}

// DeliveryOf reads the delivery state from a task's metadata; ok is false
// when the task carries none.
func DeliveryOf(meta map[string]any) (s DeliveryState, ok bool) {
	s.Delivery, _ = meta[KeyDelivery].(string)
	if s.Delivery == "" {
		return s, false
	}
	s.Detail, _ = meta[KeyDeliveryDetail].(string)
	s.Wait, _ = meta[KeyDeliveryWait].(string)
	s.Ordered, _ = meta[KeyDeliveryOrdered].(bool)
	if v, _ := meta[KeyDeliverySince].(string); v != "" {
		s.Since, _ = time.Parse(time.RFC3339, v)
	}
	if v, _ := meta[KeyDeliveryEndsAt].(string); v != "" {
		s.EndsAt, _ = time.Parse(time.RFC3339, v)
	}
	return s, true
}

// Text is the sentence of the state (DeliveryText), with the give-up moment
// when the task has one.
func (s DeliveryState) Text(lang Language) string {
	return DeliveryText(s.Delivery, s.Detail, s.Wait, s.Ordered, s.EndsAt, lang)
}

// DeliveryLine is line 2 of the human lines of a task command (ux UX-C2,
// DLV-L-2): the sentence of a delivery that is pending or unconfirmed, with
// how long it has been waited for appended, "(已等 {dur})" / "(waited {dur})".
// While pending the time is from the message to now; once unconfirmed, from
// the message to the moment the task ended; when the projection does not say
// either, the sentence stands without it (UX-G8). ok is false for any other
// state.
func DeliveryLine(t Task, lang Language, nowMS int64) (string, bool) {
	s, ok := DeliveryOf(t.Metadata)
	if !ok || (s.Delivery != DeliveryPending && s.Delivery != DeliveryUnconfirmed) {
		return "", false
	}
	text := s.Text(lang)
	if text == "" {
		return "", false
	}
	var until time.Time
	switch s.Delivery {
	case DeliveryPending:
		until = time.UnixMilli(nowMS)
	default:
		if t.Status.Timestamp != nil && t.Status.State.Terminal() {
			until = *t.Status.Timestamp
		}
	}
	if s.Since.IsZero() || until.IsZero() || until.Before(s.Since) {
		return text, true
	}
	d := humanDuration(until.Sub(s.Since), lang)
	if lang == Chinese {
		return text + "(已等 " + d + ")", true
	}
	return text + " (waited " + d + ")", true
}
