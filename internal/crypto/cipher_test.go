package crypto

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, secret := range []string{
		"admin",
		"P@ssw0rd с пробелом и кириллицей",
		"",
		strings.Repeat("длинный пароль ", 100),
	} {
		env, ver, err := c.Encrypt(secret)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", secret, err)
		}
		if ver != CurrentKeyVersion {
			t.Errorf("версия ключа %d, ожидалась %d", ver, CurrentKeyVersion)
		}
		got, err := c.Decrypt(env)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if got != secret {
			t.Errorf("расшифровано %q, ожидалось %q", got, secret)
		}
	}
}

// TestEnvelopeLayout фиксирует формат конверта: "v1:" + base64(nonce‖tag‖cipher).
//
// ⚠ Go отдаёт из Seal «cipher‖tag», а формат требует тег ВПЕРЕДИ. Если порядок
// перепутать, свои же записи читаться будут (ошибка симметрична), а чужие — нет,
// и обнаружится это только при попытке прочесть конверт, записанный монолитом.
func TestEnvelopeLayout(t *testing.T) {
	c, err := New(testKey(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	const secret = "roter-admin-2026"
	env, _, err := c.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	if !strings.HasPrefix(env, "v1:") {
		t.Fatalf("конверт должен начинаться с версии ключа, получено %q", env[:min(6, len(env))])
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(env, "v1:"))
	if err != nil {
		t.Fatalf("тело конверта не base64: %v", err)
	}
	if want := nonceSize + tagSize + len(secret); len(raw) != want {
		t.Errorf("длина конверта %d, ожидалась %d (nonce 12 + tag 16 + %d)",
			len(raw), want, len(secret))
	}
}

func TestEncryptIsNotDeterministic(t *testing.T) {
	c, _ := New(testKey(t))
	a, _, _ := c.Encrypt("одно и то же")
	b, _, _ := c.Encrypt("одно и то же")
	if a == b {
		t.Error("два шифрования одного секрета совпали — nonce не случаен")
	}
}

func TestTamperedEnvelopeRejected(t *testing.T) {
	c, _ := New(testKey(t))
	env, _, _ := c.Encrypt("секрет")

	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(env, "v1:"))
	// портим последний байт шифротекста
	raw[len(raw)-1] ^= 0xFF
	tampered := "v1:" + base64.StdEncoding.EncodeToString(raw)

	if _, err := c.Decrypt(tampered); err == nil {
		t.Fatal("подделанный конверт расшифровался — GCM не проверяет подлинность")
	}
}

func TestWrongKeyRejected(t *testing.T) {
	a, _ := New(testKey(t))
	b, _ := New(testKey(t))

	env, _, _ := a.Encrypt("секрет")
	if _, err := b.Decrypt(env); err == nil {
		t.Fatal("конверт расшифровался ЧУЖИМ ключом")
	}
}

func TestBadEnvelopes(t *testing.T) {
	c, _ := New(testKey(t))
	for _, bad := range []string{
		"", "v1", "v1:", "1:abc", "vX:abc", "v1:не-base64!!",
		"v1:" + base64.StdEncoding.EncodeToString([]byte("слишком коротко")),
	} {
		if _, err := c.Decrypt(bad); err == nil {
			t.Errorf("Decrypt(%q) не вернул ошибку", bad)
		}
	}
}

func TestUnknownKeyVersion(t *testing.T) {
	c, _ := New(testKey(t))
	env, _, _ := c.Encrypt("секрет")
	// подменяем версию на несуществующую
	future := "v9" + env[2:]
	_, err := c.Decrypt(future)
	if !errors.Is(err, ErrUnknownKey) {
		t.Errorf("ожидалась ErrUnknownKey, получено %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	oldKey := testKey(t)
	oldCipher, _ := New(oldKey)
	envelopeV1, _, _ := oldCipher.Encrypt("старый секрет")

	// после ротации текущим стал другой ключ, старый зарегистрирован под v2
	// (номера условны — важно, что расшифровка выбирает ключ по версии).
	newCipher, _ := New(testKey(t))
	if err := newCipher.RegisterOldKey(2, oldKey); err != nil {
		t.Fatalf("RegisterOldKey: %v", err)
	}
	rewritten := "v2" + envelopeV1[2:]
	got, err := newCipher.Decrypt(rewritten)
	if err != nil {
		t.Fatalf("не удалось прочитать конверт старым ключом: %v", err)
	}
	if got != "старый секрет" {
		t.Errorf("получено %q", got)
	}

	if err := newCipher.RegisterOldKey(CurrentKeyVersion, oldKey); err == nil {
		t.Error("перезапись текущей версии ключа должна отвергаться")
	}
}

func TestKeyValidation(t *testing.T) {
	if _, err := New(""); !errors.Is(err, ErrKeyMissing) {
		t.Errorf("пустой ключ: ожидалась ErrKeyMissing, получено %v", err)
	}
	if _, err := New("   "); !errors.Is(err, ErrKeyMissing) {
		t.Errorf("пробельный ключ: ожидалась ErrKeyMissing, получено %v", err)
	}
	short := base64.StdEncoding.EncodeToString([]byte("слишком короткий ключ"))
	if _, err := New(short); !errors.Is(err, ErrKeySize) {
		t.Errorf("короткий ключ: ожидалась ErrKeySize, получено %v", err)
	}
	if _, err := New("не base64 вовсе !!!"); !errors.Is(err, ErrKeySize) {
		t.Errorf("не-base64: ожидалась ErrKeySize, получено %v", err)
	}
}

// TestErrorsDoNotLeakSecrets — сообщения об ошибках уходят в лог,
// секретам и ключам там не место.
func TestErrorsDoNotLeakSecrets(t *testing.T) {
	key := testKey(t)
	c, _ := New(key)
	const secret = "оченьСекретныйПароль123"
	env, _, _ := c.Encrypt(secret)

	other, _ := New(testKey(t))
	_, err := other.Decrypt(env)
	if err == nil {
		t.Fatal("ожидалась ошибка расшифровки")
	}
	msg := err.Error()
	if strings.Contains(msg, secret) || strings.Contains(msg, key) || strings.Contains(msg, env) {
		t.Errorf("в тексте ошибки утёк секрет, ключ или конверт: %q", msg)
	}

	_, err = New(base64.StdEncoding.EncodeToString([]byte("короткий")))
	if err != nil && strings.Contains(err.Error(), "короткий") {
		t.Errorf("в тексте ошибки утёк материал ключа: %q", err.Error())
	}
}

func TestGenerateKeyIs32Bytes(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(k)
	if err != nil {
		t.Fatalf("сгенерированный ключ не base64: %v", err)
	}
	if len(raw) != keySize {
		t.Errorf("ключ %d байт, ожидалось %d", len(raw), keySize)
	}
	if a, _ := GenerateKey(); a == k {
		t.Error("два вызова GenerateKey вернули одинаковый ключ")
	}
}
