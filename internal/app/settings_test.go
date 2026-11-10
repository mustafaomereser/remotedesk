package app

import (
	"encoding/hex"
	"testing"
)

func TestCheckPassword(t *testing.T) {
	a := &App{}
	a.st.Password = "tekkullanim"
	salt := hex.EncodeToString([]byte("0123456789abcdef"))
	a.set.FixedSalt = salt
	a.set.FixedHash = fixedHash("sabitsifre123", salt)

	if ok, un := a.checkPassword("tekkullanim"); !ok || un {
		t.Errorf("tek kullanımlık şifre: ok=%v unattended=%v", ok, un)
	}
	if ok, un := a.checkPassword("sabitsifre123"); !ok || !un {
		t.Errorf("sabit şifre gözetimsiz olmalı: ok=%v unattended=%v", ok, un)
	}
	if ok, _ := a.checkPassword("yanlis"); ok {
		t.Error("yanlış şifre kabul edildi")
	}
	if ok, _ := a.checkPassword(""); ok {
		t.Error("boş şifre kabul edildi")
	}
	a.set.FixedHash = ""
	if ok, _ := a.checkPassword("sabitsifre123"); ok {
		t.Error("kaldırılan sabit şifre hâlâ kabul ediliyor")
	}
}
