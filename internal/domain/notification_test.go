package domain

import (
	"testing"

	"github.com/google/uuid"
)

func TestValidateCreateItem_SMSLength(t *testing.T) {
	long := string(make([]byte, maxSMSLen+1))
	err := ValidateCreateItem(CreateItem{
		Recipient: "x",
		Channel:   ChannelSMS,
		Content:   &long,
	})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateCreateItem_TemplateRequiresPayload(t *testing.T) {
	tid := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	err := ValidateCreateItem(CreateItem{
		Recipient:  "x",
		Channel:    ChannelEmail,
		TemplateID: &tid,
	})
	if err == nil {
		t.Fatal("expected error")
	}
}
