package controller

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"k8s.io/utils/ptr"

	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/cloudflare"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/config"
	"go.pkgs.davy.tw/cf-tunnel-gateway/internal/translate"
)

// dnsSyncer publishes a Gateway's hostnames as proxied CNAME records to its
// tunnel, each marked by an ownership TXT record.
type dnsSyncer struct {
	CF     *cloudflare.Client
	Config *config.Config
	Token  string
	// Gateway is "namespace/name".
	Gateway string
	// Target is the tunnel the records point at.
	Target string
}

// dnsState is the outcome for one hostname.
type dnsState struct {
	Ready   bool
	Message string
}

// heritage is the fixed part of every ownership TXT record.
const heritage = "heritage=cf-tunnel-gateway"

// txtName is the name of a hostname's ownership TXT record.
func txtName(prefix, host string) string {
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		return prefix + "-wildcard." + rest
	}
	return prefix + "." + host
}

// ownerContent is the TXT content marking a record as this Gateway's.
func (s *dnsSyncer) ownerContent() string {
	return fmt.Sprintf("%s,owner=%s,gateway=%s", heritage, s.Config.DNSOwnerID, s.Gateway)
}

// txtOwner parses an ownership TXT record's content into owner ID and
// Gateway. ok is false for foreign records.
func txtOwner(content string) (owner, gateway string, ok bool) {
	content = strings.Trim(content, `"`)
	fields := map[string]string{}
	for _, f := range strings.Split(content, ",") {
		k, v, _ := strings.Cut(strings.TrimSpace(f), "=")
		fields[k] = v
	}
	if "heritage="+fields["heritage"] != heritage {
		return "", "", false
	}
	return fields["owner"], fields["gateway"], true
}

func (s *dnsSyncer) comment() string {
	c := s.Config.DNSComment
	if c != "" {
		c += " "
	}
	c += "(" + s.Gateway + ")"
	if len(c) > 100 {
		c = c[:100]
	}
	return c
}

// Sync makes the records of hosts point at the target tunnel and, under
// the sync policy, removes owned records of the hostnames in previous that
// are no longer published. It returns per-hostname states and the
// hostnames whose records this Gateway may still own (to record for later
// cleanup). Errors that concern one hostname are reported in its state;
// the returned error is for Cloudflare failures that should requeue.
func (s *dnsSyncer) Sync(ctx context.Context, hosts []translate.Hostname, previous []string) (map[string]dnsState, []string, error) {
	states := map[string]dnsState{}
	keep := map[string]bool{}
	var firstErr error
	for _, h := range hosts {
		st, owned, err := s.ensure(ctx, h)
		if err != nil {
			if _, limited := cloudflare.RetryAfter(err); limited {
				return nil, nil, err
			}
			st = dnsState{Message: fmt.Sprintf("updating DNS records of %s: %v", h.Name, err)}
			if firstErr == nil {
				firstErr = err
			}
			owned = true // unknown: keep it recorded so cleanup retries
		}
		states[h.Name] = st
		if owned {
			keep[h.Name] = true
		}
	}
	current := map[string]bool{}
	for _, h := range hosts {
		current[h.Name] = true
	}
	for _, name := range previous {
		if current[name] {
			continue
		}
		if s.Config.DNSPolicy == config.DNSPolicyUpsertOnly {
			continue
		}
		if err := s.remove(ctx, name); err != nil {
			if _, limited := cloudflare.RetryAfter(err); limited {
				return nil, nil, err
			}
			keep[name] = true
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if s.Config.DNSPolicy == config.DNSPolicyUpsertOnly {
		// Records are never deleted, so there is nothing to remember.
		keep = map[string]bool{}
	}
	out := make([]string, 0, len(keep))
	for k := range keep {
		out = append(out, k)
	}
	sort.Strings(out)
	return states, out, firstErr
}

// ensure publishes one hostname. owned reports whether this Gateway owns
// its records afterwards.
func (s *dnsSyncer) ensure(ctx context.Context, h translate.Hostname) (dnsState, bool, error) {
	zone, err := findZone(ctx, s.CF, s.Token, "", h.Name)
	if err != nil {
		return dnsState{}, false, err
	}
	if zone == nil {
		return dnsState{Message: fmt.Sprintf("no active Cloudflare zone visible to the DNS API token contains %s", h.Name)}, false, nil
	}
	tname := txtName(s.Config.DNSTXTPrefix, h.Name)
	txts, err := s.CF.ListDNSRecords(ctx, s.Token, zone.ID, tname, "TXT")
	if err != nil {
		return dnsState{}, false, err
	}
	all, err := s.CF.ListDNSRecords(ctx, s.Token, zone.ID, h.Name, "")
	if err != nil {
		return dnsState{}, false, err
	}
	var txt *cloudflare.DNSRecord
	otherGateway := ""
	for i := range txts {
		owner, gw, ok := txtOwner(txts[i].Content)
		if !ok || owner != s.Config.DNSOwnerID {
			continue
		}
		if gw == s.Gateway {
			txt = &txts[i]
			otherGateway = ""
			break
		}
		otherGateway = gw
		if h.DNSParent == s.Gateway && slices.Contains(h.DNSFrom, gw) {
			// A switching route attached to both Gateways names this one
			// as the new owner.
			txt = &txts[i]
		}
	}
	if otherGateway != "" && txt == nil {
		return dnsState{Message: fmt.Sprintf("DNS records of %s are owned by Gateway %s", h.Name, otherGateway)}, false, nil
	}
	var cname *cloudflare.DNSRecord
	for i := range all {
		r := &all[i]
		switch r.Type {
		case "CNAME":
			cname = r
		case "A", "AAAA":
			if txt == nil {
				return dnsState{Message: fmt.Sprintf("%s already has %s records that this controller does not own", h.Name, r.Type)}, false, nil
			}
		}
	}
	if txt == nil && cname != nil {
		return dnsState{Message: fmt.Sprintf("%s already has a CNAME record (to %s) that this controller does not own", h.Name, cname.Content)}, false, nil
	}
	// The TXT record goes first, so a crash between the two calls leaves
	// an owned name rather than an orphaned CNAME.
	want := cloudflare.DNSRecord{Type: "TXT", Name: tname, Content: `"` + s.ownerContent() + `"`, TTL: 1, Comment: s.comment()}
	if txt == nil {
		if _, err := s.CF.CreateDNSRecord(ctx, s.Token, zone.ID, want); err != nil {
			return dnsState{}, false, err
		}
	} else if strings.Trim(txt.Content, `"`) != s.ownerContent() {
		if err := s.CF.UpdateDNSRecord(ctx, s.Token, zone.ID, txt.ID, want); err != nil {
			return dnsState{}, true, err
		}
	}
	target := cloudflare.CNAMETarget(s.Target)
	rec := cloudflare.DNSRecord{Type: "CNAME", Name: h.Name, Content: target, Proxied: ptr.To(true), TTL: 1, Comment: s.comment()}
	switch {
	case cname == nil:
		// Owned A/AAAA records (from an older setup) would block the CNAME.
		for _, r := range all {
			if r.Type == "A" || r.Type == "AAAA" {
				if err := s.CF.DeleteDNSRecord(ctx, s.Token, zone.ID, r.ID); err != nil {
					return dnsState{}, true, err
				}
			}
		}
		if _, err := s.CF.CreateDNSRecord(ctx, s.Token, zone.ID, rec); err != nil {
			return dnsState{}, true, err
		}
	case !strings.EqualFold(cname.Content, target) || cname.Proxied == nil || !*cname.Proxied || cname.Comment != rec.Comment:
		if err := s.CF.UpdateDNSRecord(ctx, s.Token, zone.ID, cname.ID, rec); err != nil {
			return dnsState{}, true, err
		}
	}
	return dnsState{Ready: true, Message: fmt.Sprintf("%s points at tunnel %s", h.Name, s.Target)}, true, nil
}

// remove deletes the records of a hostname if this Gateway owns them.
func (s *dnsSyncer) remove(ctx context.Context, host string) error {
	zone, err := findZone(ctx, s.CF, s.Token, "", host)
	if err != nil || zone == nil {
		return err
	}
	tname := txtName(s.Config.DNSTXTPrefix, host)
	txts, err := s.CF.ListDNSRecords(ctx, s.Token, zone.ID, tname, "TXT")
	if err != nil {
		return err
	}
	var owned []cloudflare.DNSRecord
	for _, t := range txts {
		if owner, gw, ok := txtOwner(t.Content); ok && owner == s.Config.DNSOwnerID && gw == s.Gateway {
			owned = append(owned, t)
		}
	}
	if len(owned) == 0 {
		return nil
	}
	cnames, err := s.CF.ListDNSRecords(ctx, s.Token, zone.ID, host, "CNAME")
	if err != nil {
		return err
	}
	// The CNAME goes first; the TXT record last, so a failure in between
	// keeps the name marked as ours and the next attempt finishes it.
	for _, c := range cnames {
		if err := s.CF.DeleteDNSRecord(ctx, s.Token, zone.ID, c.ID); err != nil {
			return err
		}
	}
	for _, t := range owned {
		if err := s.CF.DeleteDNSRecord(ctx, s.Token, zone.ID, t.ID); err != nil {
			return err
		}
	}
	return nil
}

// RemoveAll deletes the records of every hostname in hosts that this
// Gateway owns.
func (s *dnsSyncer) RemoveAll(ctx context.Context, hosts []string) error {
	for _, h := range hosts {
		if err := s.remove(ctx, h); err != nil {
			return err
		}
	}
	return nil
}
