package snowflake_broker

import (
	"bytes"
	"container/heap"
	. "github.com/smartystreets/goconvey/convey"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBroker(t *testing.T) {

	Convey("Context", t, func() {
		ctx := NewBrokerContext()

		Convey("Adds Snowflake", func() {
			So(ctx.snowflakes.Len(), ShouldEqual, 0)
			So(len(ctx.snowflakeMap), ShouldEqual, 0)
			ctx.AddSnowflake("foo")
			So(ctx.snowflakes.Len(), ShouldEqual, 1)
			So(len(ctx.snowflakeMap), ShouldEqual, 1)
		})

		Convey("Responds to client offers...", func() {
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte("test"))
			r, err := http.NewRequest("POST", "snowflake.broker/client", data)
			So(err, ShouldBeNil)

			Convey("with 503 when no snowflakes are available.", func() {
				clientHandler(ctx, w, r)
				h := w.Header()
				So(h["Access-Control-Allow-Headers"], ShouldNotBeNil)
				So(w.Code, ShouldEqual, http.StatusServiceUnavailable)
				So(w.Body.String(), ShouldEqual, "")
			})

			Convey("with a proxy answer if available.", func() {
				done := make(chan bool)
				// Prepare a fake proxy to respond with.
				snowflake := ctx.AddSnowflake("fake")
				go func() {
					clientHandler(ctx, w, r)
					done <- true
				}()
				offer := <-snowflake.offerChannel
				So(offer, ShouldResemble, []byte("test"))
				snowflake.answerChannel <- []byte("fake answer")
				<-done
				So(w.Body.String(), ShouldEqual, "fake answer")
				So(w.Code, ShouldEqual, http.StatusOK)
			})

			Convey("Times out when no proxy responds.", func() {
				if testing.Short() {
					return
				}
				done := make(chan bool)
				snowflake := ctx.AddSnowflake("fake")
				go func() {
					clientHandler(ctx, w, r)
					done <- true
				}()
				offer := <-snowflake.offerChannel
				So(offer, ShouldResemble, []byte("test"))
				<-done
				So(w.Code, ShouldEqual, http.StatusGatewayTimeout)
			})
		})

		Convey("Responds to proxy polls...", func() {
			done := make(chan bool)
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte("test"))
			r, err := http.NewRequest("POST", "snowflake.broker/proxy", data)
			r.Header.Set("X-Session-ID", "test")
			So(err, ShouldBeNil)

			Convey("with a client offer if available.", func() {
				go func(ctx *BrokerContext) {
					proxyHandler(ctx, w, r)
					done <- true
				}(ctx)
				// Pass a fake client offer to this proxy
				p := <-ctx.createChannel
				So(p.id, ShouldEqual, "test")
				p.offerChan <- []byte("fake offer")
				<-done
				So(w.Code, ShouldEqual, http.StatusOK)
				So(w.Body.String(), ShouldEqual, "fake offer")
			})

			Convey("times out when no client offer is available.", func() {
				go func(ctx *BrokerContext) {
					proxyHandler(ctx, w, r)
					done <- true
				}(ctx)
				p := <-ctx.createChannel
				So(p.id, ShouldEqual, "test")
				// nil means timeout
				p.offerChan <- nil
				<-done
				So(w.Body.String(), ShouldEqual, "")
				So(w.Code, ShouldEqual, http.StatusGatewayTimeout)
			})
		})

		Convey("Responds to proxy answers...", func() {
			s := ctx.AddSnowflake("test")
			w := httptest.NewRecorder()
			data := bytes.NewReader([]byte("fake answer"))

			Convey("by passing to the client if valid.", func() {
				r, err := http.NewRequest("POST", "snowflake.broker/answer", data)
				So(err, ShouldBeNil)
				r.Header.Set("X-Session-ID", "test")
				go func(ctx *BrokerContext) {
					answerHandler(ctx, w, r)
				}(ctx)
				answer := <-s.answerChannel
				So(w.Code, ShouldEqual, http.StatusOK)
				So(answer, ShouldResemble, []byte("fake answer"))
			})

			Convey("with error if the proxy is not recognized", func() {
				r, err := http.NewRequest("POST", "snowflake.broker/answer", nil)
				So(err, ShouldBeNil)
				r.Header.Set("X-Session-ID", "invalid")
				answerHandler(ctx, w, r)
				So(w.Code, ShouldEqual, http.StatusGone)
			})

			Convey("with error if the proxy gives invalid answer", func() {
				data := bytes.NewReader(nil)
				r, err := http.NewRequest("POST", "snowflake.broker/answer", data)
				r.Header.Set("X-Session-ID", "test")
				So(err, ShouldBeNil)
				answerHandler(ctx, w, r)
				So(w.Code, ShouldEqual, http.StatusBadRequest)
			})
		})
	})

	Convey("End-To-End", t, func() {
		done := make(chan bool)
		polled := make(chan bool)
		ctx := NewBrokerContext()

		// Proxy polls with its ID first...
		dataP := bytes.NewReader([]byte("test"))
		wP := httptest.NewRecorder()
		rP, err := http.NewRequest("POST", "snowflake.broker/proxy", dataP)
		So(err, ShouldBeNil)
		rP.Header.Set("X-Session-ID", "test")
		go func() {
			proxyHandler(ctx, wP, rP)
			polled <- true
		}()

		// Manually do the Broker goroutine action here for full control.
		p := <-ctx.createChannel
		So(p.id, ShouldEqual, "test")
		s := ctx.AddSnowflake(p.id)
		go func() {
			offer := <-s.offerChannel
			p.offerChan <- offer
		}()
		So(ctx.snowflakeMap["test"], ShouldNotBeNil)

		// Client request blocks until proxy answer arrives.
		dataC := bytes.NewReader([]byte("fake offer"))
		wC := httptest.NewRecorder()
		rC, err := http.NewRequest("POST", "snowflake.broker/client", dataC)
		So(err, ShouldBeNil)
		go func() {
			clientHandler(ctx, wC, rC)
			done <- true
		}()

		<-polled
		So(wP.Code, ShouldEqual, http.StatusOK)
		So(wP.Body.String(), ShouldResemble, "fake offer")
		So(ctx.snowflakeMap["test"], ShouldNotBeNil)
		// Follow up with the answer request afterwards
		wA := httptest.NewRecorder()
		dataA := bytes.NewReader([]byte("fake answer"))
		rA, err := http.NewRequest("POST", "snowflake.broker/proxy", dataA)
		So(err, ShouldBeNil)
		rA.Header.Set("X-Session-ID", "test")
		answerHandler(ctx, wA, rA)
		So(wA.Code, ShouldEqual, http.StatusOK)

		<-done
		So(wC.Code, ShouldEqual, http.StatusOK)
		So(wC.Body.String(), ShouldEqual, "fake answer")
	})
}

func TestSnowflakeHeap(t *testing.T) {
	Convey("SnowflakeHeap", t, func() {
		h := new(SnowflakeHeap)
		heap.Init(h)
		So(h.Len(), ShouldEqual, 0)
		s1 := new(Snowflake)
		s2 := new(Snowflake)
		s3 := new(Snowflake)
		s4 := new(Snowflake)
		s1.clients = 4
		s2.clients = 5
		s3.clients = 3
		s4.clients = 1

		heap.Push(h, s1)
		So(h.Len(), ShouldEqual, 1)
		heap.Push(h, s2)
		So(h.Len(), ShouldEqual, 2)
		heap.Push(h, s3)
		So(h.Len(), ShouldEqual, 3)
		heap.Push(h, s4)
		So(h.Len(), ShouldEqual, 4)

		heap.Remove(h, 0)
		So(h.Len(), ShouldEqual, 3)

		r := heap.Pop(h).(*Snowflake)
		So(h.Len(), ShouldEqual, 2)
		So(r.clients, ShouldEqual, 3)
		So(r.index, ShouldEqual, -1)

		r = heap.Pop(h).(*Snowflake)
		So(h.Len(), ShouldEqual, 1)
		So(r.clients, ShouldEqual, 4)
		So(r.index, ShouldEqual, -1)

		r = heap.Pop(h).(*Snowflake)
		So(h.Len(), ShouldEqual, 0)
		So(r.clients, ShouldEqual, 5)
		So(r.index, ShouldEqual, -1)
	})
}