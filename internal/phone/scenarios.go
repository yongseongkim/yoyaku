package phone

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Scenario struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Tag         string `json:"tag"`
}

var Scenarios = []Scenario{{"available", "정상 예약", "원하는 시간에 자리가 있어요.", "기본"}, {"alternative", "대체 시간 제안", "30분 늦은 시간을 제안해요.", "시간 변경"}, {"fully_booked", "만석", "예약할 수 있는 자리가 없어요.", "예약 불가"}, {"clarification", "이름 재확인", "이름을 한 번 더 물어봐요.", "되묻기"}, {"busy", "통화 중", "가게가 다른 전화를 받고 있어요.", "연결 실패"}, {"no_answer", "무응답", "신호가 가지만 받지 않아요.", "연결 실패"}, {"disconnect", "통화 끊김", "예약 요청 도중 통화가 끊겨요.", "중단"}, {"voicemail", "자동 응답", "영업시간 안내가 재생돼요.", "자동 응답"}}

type Reservation struct {
	Name              string `json:"name"`
	NameJA            string `json:"nameJa"`
	Date              string `json:"date"`
	Time              string `json:"time"`
	PartySize         int    `json:"partySize"`
	Phone             string `json:"phone"`
	Scenario          string `json:"scenario"`
	Revision          int    `json:"revision"`
	ProposedTime      string `json:"proposedTime"`
	Status            string `json:"status"`
	EmployeeRevision  int    `json:"employeeConfirmedRevision"`
	UserRevision      int    `json:"userApprovedRevision"`
	ConfirmedFacts    any    `json:"confirmedFacts"`
	Price             any    `json:"price"`
	CancellationTerms any    `json:"cancellationTerms"`
}

func DefaultReservation() Reservation {
	return Reservation{Name: "김민수", NameJA: "キム・ミンス", Date: "2027-01-10", Time: "19:00", PartySize: 2, Phone: "+81312345678", Scenario: "available"}
}
func validScenario(id string) bool {
	for _, s := range Scenarios {
		if s.ID == id {
			return true
		}
	}
	return false
}
func (r *Reservation) Validate() error {
	r.Name = strings.TrimSpace(r.Name)
	r.NameJA = strings.TrimSpace(r.NameJA)
	if r.NameJA == "" {
		r.NameJA = r.Name
	}
	if r.Scenario == "" {
		r.Scenario = "available"
	}
	r.Phone = strings.NewReplacer(" ", "", "(", "", ")", "", "-", "").Replace(r.Phone)
	d, e := time.Parse("2006-01-02", r.Date)
	if e != nil || d.Format("2006-01-02") != r.Date || !regexp.MustCompile(`^(?:[01]\d|2[0-2]):[0-5]\d$`).MatchString(r.Time) || r.Name == "" || len([]rune(r.Name)) > 40 || len([]rune(r.NameJA)) > 60 || r.PartySize < 1 || r.PartySize > 12 || !regexp.MustCompile(`^\+81\d{9,10}$`).MatchString(r.Phone) || !validScenario(r.Scenario) {
		return errors.New("예약 이름, 날짜, 시간, 인원, 전화번호와 시나리오를 확인해 주세요.")
	}
	return nil
}
func details(r Reservation, clock string) (string, string) {
	d, _ := time.Parse("2006-01-02", r.Date)
	h, _ := strconv.Atoi(clock[:2])
	m, _ := strconv.Atoi(clock[3:])
	km, jm := "", ""
	if m != 0 {
		km = fmt.Sprintf(" %d분", m)
		jm = fmt.Sprintf("%d分", m)
	}
	return fmt.Sprintf("%d월 %d일 %d시%s, %d명", d.Month(), d.Day(), h, km, r.PartySize), fmt.Sprintf("%d月%d日%d時%s、%d名", d.Month(), d.Day(), h, jm, r.PartySize)
}

type Phrase struct {
	Action string `json:"action"`
	KO     string `json:"ko"`
	JA     string `json:"ja"`
}

func phrase(r Reservation, action string) Phrase {
	ko, ja := details(r, r.Time)
	p := map[string][2]string{"request": {ko + " 예약하고 싶어요.", ja + "で予約をお願いしたいです。"}, "accept": {"네, 제안해 주신 시간으로 부탁드려요.", "はい、ご提案の時間でお願いします。"}, "decline": {"그 시간은 어려워요. 다른 시간도 있나요?", "その時間は難しいです。ほかの時間はありますか。"}, "name": {"예약자 이름은 " + r.Name + "입니다.", "名前は" + r.NameJA + "です。"}, "confirm_details": {"네, 안내해 주신 날짜와 시간, 인원이 맞습니다.", "はい、日付と時間、人数はその内容で間違いありません。"}, "repeat": {"다시 한 번 말씀해 주세요.", "もう一度お願いします。"}, "wait": {"잠시만 기다려 주세요.", "少々お待ちください。"}, "thanks": {"감사합니다.", "ありがとうございます。"}}[action]
	return Phrase{action, p[0], p[1]}
}
func suggestions(r Reservation, stage string) []Phrase {
	actions := map[string][]string{"greeting": {"request"}, "offer": {"accept", "decline"}, "name": {"name"}, "details": {"confirm_details"}, "confirmed": {"thanks"}, "unavailable": {"thanks"}}[stage]
	result := []Phrase{}
	if len(actions) > 0 {
		actions = append(append([]string{}, actions...), "repeat", "wait")
	}
	for _, a := range actions {
		result = append(result, phrase(r, a))
	}
	return result
}

type Fixture struct {
	JA           string         `json:"ja"`
	KO           string         `json:"ko"`
	Stage        string         `json:"stage"`
	ProposedTime string         `json:"proposedTime"`
	Outcome      string         `json:"outcome,omitempty"`
	Confirmed    bool           `json:"employeeConfirmed,omitempty"`
	Facts        map[string]any `json:"facts,omitempty"`
}
type Shop struct {
	R            Reservation
	Stage, Clock string
	asked        bool
	offers       int
	last         Fixture
}

func (s *Shop) say(ja, ko string) Fixture {
	f := Fixture{JA: ja, KO: ko, Stage: s.Stage, ProposedTime: s.Clock}
	s.last = f
	return f
}
func (s *Shop) Start() (Fixture, bool) {
	s.Stage = "greeting"
	if s.R.Scenario == "voicemail" {
		s.Stage = "unavailable"
		f := s.say("お電話ありがとうございます。ただいま営業時間外です。", "전화 주셔서 감사합니다. 지금은 영업시간이 아닙니다.")
		f.Outcome = "voicemail"
		return f, true
	}
	return s.say("お電話ありがとうございます。さくら食堂です。", "전화 주셔서 감사합니다. 사쿠라 식당입니다."), false
}
func (s *Shop) Receive(ja string) (Fixture, bool) {
	if ja == "もう一度お願いします。" {
		return s.last, false
	}
	if ja == "少々お待ちください。" {
		return Fixture{JA: "はい、お待ちしております。", KO: "네, 기다리겠습니다.", Stage: s.Stage, ProposedTime: s.Clock}, false
	}
	if ja == "ありがとうございます。" {
		return Fixture{}, true
	}
	switch s.Stage {
	case "greeting":
		if ja != phrase(s.R, "request").JA {
			return s.say("ご希望の日付と人数をもう一度お願いします。", "원하시는 날짜와 인원을 다시 말씀해 주세요."), false
		}
		switch s.R.Scenario {
		case "disconnect":
			return s.say("ご予約ですね。少々お待ちください。", "예약 말씀이시죠. 잠시만 기다려 주세요."), true
		case "fully_booked":
			s.Stage = "unavailable"
			f := s.say("申し訳ございません。その日は満席です。", "죄송합니다. 그날은 모든 자리가 예약되어 있습니다.")
			f.Outcome = "unavailable"
			return f, false
		case "alternative":
			return s.offer(), false
		}
		s.Clock = s.R.Time
		s.Stage = "name"
		return s.say("お席をご用意できます。お名前をお願いします。", "예약 가능합니다. 성함을 알려 주세요."), false
	case "offer":
		if ja == phrase(s.R, "decline").JA {
			return s.offer(), false
		}
		if ja != phrase(s.R, "accept").JA {
			return s.say("ご提案の時間でよろしいでしょうか。", "제안해 드린 시간으로 괜찮으실까요?"), false
		}
		s.Stage = "name"
		return s.say("ありがとうございます。お名前をお願いします。", "감사합니다. 성함을 알려 주세요."), false
	case "name":
		if ja != phrase(s.R, "name").JA {
			return s.say("お名前をもう一度お願いします。", "성함을 다시 말씀해 주세요."), false
		}
		if s.R.Scenario == "clarification" && !s.asked {
			s.asked = true
			return s.say("恐れ入りますが、お名前をもう一度お願いします。", "죄송하지만 성함을 한 번만 더 말씀해 주시겠어요?"), false
		}
		s.Stage = "details"
		ko, ja := details(s.R, s.Clock)
		return s.say(ja+"、"+s.R.NameJA+"様ですね。お間違いないでしょうか。", ko+", "+s.R.Name+" 님으로 예약하겠습니다. 내용이 맞으실까요?"), false
	case "details":
		if ja == phrase(s.R, "confirm_details").JA {
			s.Stage = "confirmed"
			f := s.say("ご予約を承りました。当日お待ちしております。", "예약을 접수했습니다. 당일에 뵙겠습니다.")
			f.Confirmed = true
			return f, false
		}
	}
	return s.say("もう一度お願いします。", "다시 한 번 말씀해 주세요."), false
}
func (s *Shop) offer() Fixture {
	if s.offers >= 2 {
		s.Stage = "unavailable"
		f := s.say("申し訳ございません。ほかの空きはございません。", "죄송합니다. 다른 빈 시간은 없습니다.")
		f.Outcome = "unavailable"
		return f
	}
	s.offers++
	t, _ := time.Parse("15:04", s.R.Time)
	s.Clock = t.Add(time.Duration(s.offers) * 30 * time.Minute).Format("15:04")
	s.Stage = "offer"
	return s.say("ご希望の時間は満席です。"+s.Clock+"はいかがでしょうか。", "원하시는 시간은 만석입니다. "+s.Clock+"은 어떠세요?")
}
