package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"deltav/internal/apiclient"
)

// The Controls page's forms. Each handler turns a posted form into one API
// call, then renders the Controls page again with the outcome at its top.
// With htmx the page's main part is replaced in place; without script the
// browser shows the same page as the answer to the post.

// actionFunc does what a form asks and says what happened. It may change
// the view that is about to be rendered, to hand a failed form its fields
// back.
type actionFunc func(ctx context.Context, form url.Values, v *view) (done string, err error)

var controlsSpec = pages[3]

func (u *UI) action(do actionFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "the form could not be read", http.StatusBadRequest)
			return
		}
		// The form's own outcome first, then the page as it now stands.
		echo := &view{Controls: &controlsView{}}
		done, err := do(r.Context(), r.PostForm, echo)
		v := u.build(r.Context(), controlsSpec, nil)
		status := http.StatusOK
		if err != nil {
			status = http.StatusUnprocessableEntity
			v.Flash = &flash{Text: err.Error()}
			v.Controls.NewCluster, v.Controls.NewTarget, v.Controls.OpenForm = echo.Controls.NewCluster, echo.Controls.NewTarget, echo.Controls.OpenForm
		} else {
			v.Flash = &flash{OK: true, Text: done}
		}
		u.render(w, status, "layout", v)
	}
}

// field returns a form field with surrounding space removed.
func field(form url.Values, name string) string { return strings.TrimSpace(form.Get(name)) }

// number reads a whole number between lo and hi from a form field.
func number(form url.Values, name, what string, lo, hi uint64) (uint64, error) {
	n, err := strconv.ParseUint(field(form, name), 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s: enter a whole number from %d to %d", what, lo, hi)
	}
	return n, nil
}

func (u *UI) setLimit(ctx context.Context, form url.Values, _ *view) (string, error) {
	target, mode := field(form, "target"), field(form, "mode")
	if mode != "alert" && mode != "enforce" {
		return "", errors.New("mode: choose alert or enforce")
	}
	n, err := number(form, "per_cnode", "Connections per CNode", 1, 1<<32-1)
	if err != nil {
		return "", err
	}
	if err := u.api.SetLimit(ctx, target, uint32(n), mode); err != nil {
		return "", err
	}
	if mode == "enforce" {
		return fmt.Sprintf("%s: limit set to %s connections per CNode and enforced. The new cap is on its way to every Envoy.", target, count(n)), nil
	}
	return fmt.Sprintf("%s: limit set to %s connections per CNode. CNodes over it are reported; nothing is enforced.", target, count(n)), nil
}

func (u *UI) clearLimit(ctx context.Context, form url.Values, _ *view) (string, error) {
	target := field(form, "target")
	if err := u.api.SetLimit(ctx, target, 0, ""); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s: limit removed.", target), nil
}

func (u *UI) setDrained(ctx context.Context, form url.Values, _ *view) (string, error) {
	cluster, cnode := field(form, "cluster"), field(form, "cnode")
	drain := form.Get("drained") == "true"
	if err := u.api.SetDrained(ctx, cluster, cnode, drain); err != nil {
		return "", err
	}
	if drain {
		return fmt.Sprintf("%s is drained: its VIPs are withheld from every Envoy. Nothing was changed on the VAST cluster.", cnode), nil
	}
	return fmt.Sprintf("%s is served to Envoys again.", cnode), nil
}

func (u *UI) addCluster(ctx context.Context, form url.Values, v *view) (string, error) {
	in := apiclient.NewCluster{
		Name: field(form, "name"), Host: field(form, "host"), User: field(form, "user"),
		Password: form.Get("password"), Tenant: field(form, "tenant"), Insecure: form.Get("insecure") == "true",
	}
	if err := u.api.AddCluster(ctx, in); err != nil {
		// Hand the form back, without the password.
		in.Password = ""
		v.Controls.NewCluster, v.Controls.OpenForm = in, "cluster"
		return "", err
	}
	return fmt.Sprintf("Cluster %s attached. It is read from now on; nothing on it is changed.", in.Name), nil
}

func (u *UI) removeCluster(ctx context.Context, form url.Values, _ *view) (string, error) {
	name := field(form, "name")
	if err := u.api.RemoveCluster(ctx, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("Cluster %s detached. Its stored credentials are deleted.", name), nil
}

func (u *UI) addTarget(ctx context.Context, form url.Values, v *view) (string, error) {
	in := apiclient.NewTarget{
		Name: field(form, "name"), Cluster: field(form, "cluster"), Pool: field(form, "pool"), Tenant: field(form, "tenant"),
	}
	if form.Get("tls") == "true" {
		in.TLS = &apiclient.TargetTLS{
			ServerName: field(form, "server_name"), CAFile: field(form, "ca_file"), Insecure: form.Get("tls_insecure") == "true",
		}
	}
	fail := func(err error) (string, error) {
		v.Controls.NewTarget, v.Controls.OpenForm = in, "target"
		return "", err
	}
	port, err := number(form, "port", "S3 port on the VIPs", 1, 65535)
	if err != nil {
		return fail(err)
	}
	in.Port = uint32(port)
	listen, err := number(form, "listen_port", "Loopback port on the clients", 1, 65535)
	if err != nil {
		return fail(err)
	}
	in.ListenPort = uint32(listen)
	if err := u.api.AddTarget(ctx, in); err != nil {
		return fail(err)
	}
	return fmt.Sprintf("Target %s added. Every Envoy now listens on 127.0.0.1:%d for it.", in.Name, in.ListenPort), nil
}

func (u *UI) removeTarget(ctx context.Context, form url.Values, _ *view) (string, error) {
	name := field(form, "name")
	if err := u.api.RemoveTarget(ctx, name); err != nil {
		return "", err
	}
	return fmt.Sprintf("Target %s removed. Envoys close its listener.", name), nil
}
