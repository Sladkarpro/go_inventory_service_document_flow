package helper

import (
	"net/http"
	"testing"
)

// TestClientIPНеПоддаётсяПодмене — адрес в журнале просмотров паролей нельзя
// подставить заголовком.
//
// Почему это тест, а не комментарий: журнал wh_credential_view_log — единственный
// след того, кто видел пароль от техники, и он сохраняется даже при удалении
// доступа. Подставной адрес означает, что аудит указывает не на того человека,
// причём заметить это невозможно.
//
// Раскладка заголовков воспроизводит nginx фронта:
//
//	proxy_set_header X-Real-IP        $remote_addr;
//	proxy_set_header X-Forwarded-For  $proxy_add_x_forwarded_for;
//
// то есть настоящий адрес ДОПИСЫВАЕТСЯ в конец X-Forwarded-For, а присланное
// клиентом остаётся первым элементом.
func TestClientIPCannotBeSpoofed(t *testing.T) {
	const real = "203.0.113.7" // адрес, который увидел nginx
	const fake = "10.0.0.99"   // что подставил клиент

	cases := []struct {
		name    string
		headers map[string]string
		remote  string
		want    string
	}{
		{
			name: "клиент подставил X-Forwarded-For, nginx дописал настоящий",
			// Именно этот случай раньше давал 10.0.0.99: брался первый элемент.
			headers: map[string]string{
				"X-Forwarded-For": fake + ", " + real,
				"X-Real-IP":       real,
			},
			remote: "172.18.0.5:41234", // адрес контейнера nginx
			want:   real,
		},
		{
			name: "клиент подставил и X-Real-IP — nginx его перезаписывает",
			headers: map[string]string{
				"X-Forwarded-For": fake + ", " + real,
				"X-Real-IP":       real,
			},
			remote: "172.18.0.5:41234",
			want:   real,
		},
		{
			name: "цепочка из нескольких подставных адресов",
			headers: map[string]string{
				"X-Forwarded-For": "1.2.3.4, " + fake + ", " + real,
				"X-Real-IP":       real,
			},
			remote: "172.18.0.5:41234",
			want:   real,
		},
		{
			name: "только X-Forwarded-For, без X-Real-IP",
			// Другой прокси, X-Real-IP не выставляет: настоящий адрес всё равно
			// последний, потому что его дописывает прокси, а не клиент.
			headers: map[string]string{"X-Forwarded-For": fake + ", " + real},
			remote:  "172.18.0.5:41234",
			want:    real,
		},
		{
			name:    "мусор в заголовке не проходит",
			headers: map[string]string{"X-Forwarded-For": "не-адрес", "X-Real-IP": "тоже-не-адрес"},
			remote:  "198.51.100.4:5555",
			want:    "198.51.100.4",
		},
		{
			name:    "без заголовков берётся адрес соединения",
			headers: nil,
			remote:  "198.51.100.4:5555",
			want:    "198.51.100.4",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "/spa/api/warehouse/credentials/1/reveal", nil)
			if err != nil {
				t.Fatalf("запрос: %v", err)
			}
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			req.RemoteAddr = c.remote

			got := ClientIP(req)
			if got == nil {
				t.Fatalf("адрес не определён, ожидался %s", c.want)
			}
			if *got != c.want {
				t.Errorf("в журнал попал %s, ожидался %s", *got, c.want)
			}
			if *got == fake {
				t.Error("в журнал просмотров паролей попал подставленный клиентом адрес")
			}
		})
	}
}

// TestClientIPНетАдреса — если адрес определить нельзя, возвращается nil,
// а не пустая строка: в журнале лучше отсутствие адреса, чем правдоподобная
// пустышка.
func TestClientIPReturnsNilWhenUnknown(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "не-адрес-вообще"
	if got := ClientIP(req); got != nil {
		t.Errorf("ожидался nil, получено %q", *got)
	}
}
