package dexosv1

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func resultsAt() []LogPosition {
	return []LogPosition{{509, 0}, {510, 1}, {511, 0}, {512, 0}, {513, 0}, {514, 0}}
}

func answerResults(t *testing.T, f *writeFake, requestID string, asked *url.Values) {
	f.routes["/v1/results"] = func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get(minSeqHeader) != "" {
			t.Errorf("按位置取结果应为不带 %s 的 GET,得到 %s %v", minSeqHeader, r.Method, r.Header)
		}
		f.mu.Lock()
		*asked = r.URL.Query()
		f.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(strings.ReplaceAll(string(fixture(t, "results.json")), testReqID, requestID)))
	}
}

func TestResultsAnswersEachPositionInTheOrderAskedAndParsesLikeTheSyncWrite(t *testing.T) {
	f, w, btc := newActionWriter(t, 0, 1793680000123)
	cancels, modifies, orders := replace3Inputs(btc)
	a, err := w.SignReplace3(btc, cancels, modifies, orders, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	f.answer(t, "/v1/orders/replace", 200, "w_replace3.json")
	sync, err := w.Client.SendReplace(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	var asked url.Values
	answerResults(t, f, a.Request.RequestID, &asked)
	got, err := w.Client.Results(context.Background(), resultsAt(), 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if asked.Get("at") != "509:0,510:1,511:0,512:0,513:0,514:0" || asked.Get("waitMs") != "1500" {
		t.Errorf("查询串: %v", asked)
	}
	f.mu.Unlock()
	var status []ResultStatus
	for i, r := range got {
		if r.LogPosition != resultsAt()[i] {
			t.Fatalf("第 %d 项的位置 %+v 不是问的 %+v", i, r.LogPosition, resultsAt()[i])
		}
		status = append(status, r.Status)
	}
	want := []ResultStatus{ResultExecuted, ResultRejected, ResultPending, ResultNotFound, ResultEvicted, ResultExecuted}
	if !reflect.DeepEqual(status, want) {
		t.Fatalf("结论: %v", status)
	}

	batch, err := got[0].Batch(a)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(batch.Items, sync.Items) {
		t.Fatalf("逐项结果应与同步写解析出的相同:\n got %+v\nwant %+v", batch.Items, sync.Items)
	}
	if batch.Seq != 509 || batch.Sub != 0 || batch.Duplicate || batch.Request != a.Request {
		t.Fatalf("回执: %+v", batch.Receipt)
	}
	dup, err := got[5].Batch(a)
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Duplicate || dup.Seq != 509 || dup.Sub != 0 {
		t.Fatalf("重发的结果按原请求的位置给,同同步写: %+v", dup.Receipt)
	}

	_, err = got[1].Batch(a)
	var e *Error
	if !errors.As(err, &e) || e.Status != http.StatusUnprocessableEntity || e.Code != "InsufficientMargin" || e.Num != 20011 {
		t.Fatalf("整笔被拒应是同步写 422 那个错误: %v", err)
	}
	if ClassifyAction(err) != ActionRejected {
		t.Fatalf("整笔被拒的判法同同步写: %v", ClassifyAction(err))
	}
	for _, r := range got[2:5] {
		if _, err := r.Batch(a); err == nil {
			t.Fatalf("%s 没有结果可解析", r.Status)
		}
	}
	if !strings.Contains(got[4].Message, "/v1/requests/") {
		t.Fatalf("窗口外要指给按请求身份查: %+v", got[4])
	}
}

func TestResultsRefusesBadAsksAndAnswersThatDoNotMatch(t *testing.T) {
	f, w, btc := newActionWriter(t, 0, 1793680000123)
	f.routes["/v1/results"] = func(http.ResponseWriter, *http.Request) {
		t.Errorf("问法不对的不该发出去")
	}
	many := make([]LogPosition, 65)
	for name, ask := range map[string]func() ([]Result, error){
		"没有位置":    func() ([]Result, error) { return w.Client.Results(context.Background(), nil, 0) },
		"超过 64 个": func() ([]Result, error) { return w.Client.Results(context.Background(), many, 0) },
		"等待超过 5 秒": func() ([]Result, error) {
			return w.Client.Results(context.Background(), resultsAt(), 5001*time.Millisecond)
		},
		"等待为负": func() ([]Result, error) {
			return w.Client.Results(context.Background(), resultsAt(), -time.Millisecond)
		},
	} {
		if _, err := ask(); err == nil {
			t.Fatalf("%s: 应在发出前就拒", name)
		}
	}

	var asked url.Values
	answerResults(t, f, testReqID, &asked)
	if _, err := w.Client.Results(context.Background(), resultsAt()[:5], 0); err == nil {
		t.Fatal("答的项数与问的不同要报错")
	}
	swapped := resultsAt()
	swapped[0], swapped[1] = swapped[1], swapped[0]
	if _, err := w.Client.Results(context.Background(), swapped, 0); err == nil {
		t.Fatal("答的位置与问的对不上要报错")
	}

	cancels, modifies, orders := replace3Inputs(btc)
	a, err := w.SignReplace3(btc, cancels, modifies, orders, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Client.Results(context.Background(), resultsAt(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := got[0].Batch(a); err == nil {
		t.Fatal("结果的请求身份不是这次签的请求,要报错")
	}

	f.routes["/v1/results"] = serve(t, 400, "err404.json")
	if _, err := w.Client.Results(context.Background(), resultsAt(), 0); err == nil {
		t.Fatal("非 200 要报错")
	}
}
