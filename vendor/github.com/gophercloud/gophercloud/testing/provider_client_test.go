package testing

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gophercloud/gophercloud"
	th "github.com/gophercloud/gophercloud/testhelper"
	"github.com/gophercloud/gophercloud/testhelper/client"
)

func TestAuthenticatedHeaders(t *testing.T) {
	p := &gophercloud.ProviderClient{
		TokenID: "1234",
	}
	expected := map[string]string{"X-Auth-Token": "1234"}
	actual := p.AuthenticatedHeaders()
	th.CheckDeepEquals(t, expected, actual)
}

func TestUserAgent(t *testing.T) {
	p := &gophercloud.ProviderClient{}

	p.UserAgent.Prepend("custom-user-agent/2.4.0")
	expected := "custom-user-agent/2.4.0 gophercloud/2.0.0"
	actual := p.UserAgent.Join()
	th.CheckEquals(t, expected, actual)

	p.UserAgent.Prepend("another-custom-user-agent/0.3.0", "a-third-ua/5.9.0")
	expected = "another-custom-user-agent/0.3.0 a-third-ua/5.9.0 custom-user-agent/2.4.0 gophercloud/2.0.0"
	actual = p.UserAgent.Join()
	th.CheckEquals(t, expected, actual)

	p.UserAgent = gophercloud.UserAgent{}
	expected = "gophercloud/2.0.0"
	actual = p.UserAgent.Join()
	th.CheckEquals(t, expected, actual)
}

func TestConcurrentReauth(t *testing.T) {
	var info = struct {
		numreauths  int
		failedAuths int
		mut         *sync.RWMutex
	}{
		0,
		0,
		new(sync.RWMutex),
	}

	numconc := 20

	prereauthTok := client.TokenID
	postreauthTok := "12345678"

	p := new(gophercloud.ProviderClient)
	p.UseTokenLock()
	p.SetToken(prereauthTok)
	p.ReauthFunc = func() error {
		p.IsThrowaway = true
		time.Sleep(1 * time.Second)
		p.AuthenticatedHeaders()
		info.mut.Lock()
		info.numreauths++
		info.mut.Unlock()
		p.TokenID = postreauthTok
		p.IsThrowaway = false
		return nil
	}

	th.SetupHTTP()
	defer th.TeardownHTTP()

	th.Mux.HandleFunc("/route", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != postreauthTok {
			w.WriteHeader(http.StatusUnauthorized)
			info.mut.Lock()
			info.failedAuths++
			info.mut.Unlock()
			return
		}
		info.mut.RLock()
		hasReauthed := info.numreauths != 0
		info.mut.RUnlock()

		if hasReauthed {
			th.CheckEquals(t, p.Token(), postreauthTok)
		}

		w.Header().Add("Content-Type", "application/json")
		fmt.Fprintf(w, `{}`)
	})

	wg := new(sync.WaitGroup)
	reqopts := new(gophercloud.RequestOpts)
	reqopts.MoreHeaders = map[string]string{
		"X-Auth-Token": prereauthTok,
	}

	for i := 0; i < numconc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := p.Request("GET", fmt.Sprintf("%s/route", th.Endpoint()), reqopts)
			th.CheckNoErr(t, err)
			if resp == nil {
				t.Errorf("got a nil response")
				return
			}
			if resp.Body == nil {
				t.Errorf("response body was nil")
				return
			}
			defer resp.Body.Close()
			actual, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				t.Errorf("error reading response body: %s", err)
				return
			}
			th.CheckByteArrayEquals(t, []byte(`{}`), actual)
		}()
	}

	wg.Wait()

	th.AssertEquals(t, 1, info.numreauths)
}

func TestReauthEndLoop(t *testing.T) {
	var info = struct {
		reauthAttempts   int
		maxReauthReached bool
		mut              *sync.RWMutex
	}{
		0,
		false,
		new(sync.RWMutex),
	}

	numconc := 20

	p := new(gophercloud.ProviderClient)
	p.UseTokenLock()
	p.SetToken(client.TokenID)
	p.ReauthFunc = func() error {
		info.mut.Lock()
		defer info.mut.Unlock()

		if info.reauthAttempts > 5 {
			info.maxReauthReached = true
			return fmt.Errorf("Max reauthentication attempts reached")
		}
		p.IsThrowaway = true
		p.AuthenticatedHeaders()
		p.IsThrowaway = false
		info.reauthAttempts++

		return nil
	}

	th.SetupHTTP()
	defer th.TeardownHTTP()

	th.Mux.HandleFunc("/route", func(w http.ResponseWriter, r *http.Request) {
		// route always return 401
		w.WriteHeader(http.StatusUnauthorized)
		return
	})

	reqopts := new(gophercloud.RequestOpts)

	// counters for the upcoming errors
	errAfter := 0
	errUnable := 0

	wg := new(sync.WaitGroup)
	for i := 0; i < numconc; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.Request("GET", fmt.Sprintf("%s/route", th.Endpoint()), reqopts)

			// ErrErrorAfter... will happen after a successful reauthentication,
			// but the service still responds with a 401.
			if _, ok := err.(*gophercloud.ErrErrorAfterReauthentication); ok {
				errAfter++
			}

			// ErrErrorUnable... will happen when the custom reauth func reports
			// an error.
			if _, ok := err.(*gophercloud.ErrUnableToReauthenticate); ok {
				errUnable++
			}
		}()
	}

	wg.Wait()
	th.AssertEquals(t, info.reauthAttempts, 6)
	th.AssertEquals(t, info.maxReauthReached, true)
	th.AssertEquals(t, errAfter, 6)
	th.AssertEquals(t, errUnable, 14)
}

func TestRequestThatCameDuringReauthWaitsUntilItIsCompleted(t *testing.T) {
	var info = struct {
		numreauths  int
		failedAuths int
		reauthCh    chan struct{}
		mut         *sync.RWMutex
	}{
		0,
		0,
		make(chan struct{}),
		new(sync.RWMutex),
	}

	numconc := 20

	prereauthTok := client.TokenID
	postreauthTok := "12345678"

	p := new(gophercloud.ProviderClient)
	p.UseTokenLock()
	p.SetToken(prereauthTok)
	p.ReauthFunc = func() error {
		info.mut.RLock()
		if info.numreauths == 0 {
			info.mut.RUnlock()
			close(info.reauthCh)
			time.Sleep(1 * time.Second)
		} else {
			info.mut.RUnlock()
		}
		p.IsThrowaway = true
		p.AuthenticatedHeaders()
		info.mut.Lock()
		info.numreauths++
		info.mut.Unlock()
		p.TokenID = postreauthTok
		p.IsThrowaway = false
		return nil
	}

	th.SetupHTTP()
	defer th.TeardownHTTP()

	th.Mux.HandleFunc("/route", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Auth-Token") != postreauthTok {
			info.mut.Lock()
			info.failedAuths++
			info.mut.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		info.mut.RLock()
		hasReauthed := info.numreauths != 0
		info.mut.RUnlock()

		if hasReauthed {
			th.CheckEquals(t, p.Token(), postreauthTok)
		}

		w.Header().Add("Content-Type", "application/json")
		fmt.Fprintf(w, `{}`)
	})

	wg := new(sync.WaitGroup)
	reqopts := new(gophercloud.RequestOpts)
	reqopts.MoreHeaders = map[string]string{
		"X-Auth-Token": prereauthTok,
	}

	for i := 0; i < numconc; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i != 0 {
				<-info.reauthCh
			}
			resp, err := p.Request("GET", fmt.Sprintf("%s/route", th.Endpoint()), reqopts)
			th.CheckNoErr(t, err)
			if resp == nil {
				t.Errorf("got a nil response")
				return
			}
			if resp.Body == nil {
				t.Errorf("response body was nil")
				return
			}
			defer resp.Body.Close()
			actual, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				t.Errorf("error reading response body: %s", err)
				return
			}
			th.CheckByteArrayEquals(t, []byte(`{}`), actual)
		}(i)
	}

	wg.Wait()

	th.AssertEquals(t, 1, info.numreauths)
	th.AssertEquals(t, 1, info.failedAuths)
}
