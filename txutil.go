package madns

import "time"
import "github.com/miekg/dns"
import "encoding/base32"
import "github.com/hlandau/degoutils/log"
import "fmt"
import "crypto"

// Determines if a transaction should be considered to have the given query type.
// Returns true iff the query type was qtype or ANY.
func (tx *stx) istype(qtype uint16) bool {
	return tx.qtype == qtype || tx.qtype == dns.TypeANY
}

// This is used in NSEC3 hash generation. A hash like ...decafbad has one added
// to it so that it becomes ...decafbae. This is needed because NSEC3's hashes
// are inclusive-exclusive (i.e. "[,)"), and we want a hash that covers only the
// name specified.
//
// Takes a hash in base32hex form.
func stepName(hashB32Hex string) string {
	if len(hashB32Hex) == 0 {
		return ""
	}

	b, err := base32.HexEncoding.DecodeString(hashB32Hex)
	log.Panice(err, hashB32Hex)

	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 { // didn't rollover, don't need to continue
			break
		}
	}

	return base32.HexEncoding.EncodeToString(b)
}

// Returns true iff a type should be covered by a RRSIG.
func shouldSignType(t uint16, isAuthoritySection bool) bool {
	switch t {
	case dns.TypeOPT:
		return false
	case dns.TypeNS:
		return !isAuthoritySection
	default:
		return true
	}
}

// Returns true iff a client requested DNSSEC.
func (tx *stx) useDNSSEC() bool {
	if tx.e.cfg.KSK == nil {
		return false
	}

	opt := tx.req.IsEdns0()
	if opt == nil {
		return false
	}
	return opt.Do()
}

// Sets an rcode for the response if there is no error rcode currently set for
// the response. The idea is to return the rcode corresponding to the first
// error which occurs.
func (tx *stx) setRcode(x int) {
	if tx.rcode == 0 {
		tx.rcode = x
	}
}

// Determines the maximum TTL for a slice of resource records.
// Returns 0 if the slice is empty.
func rraMaxTTL(rra []dns.RR) uint32 {
	x := uint32(0)
	for _, rr := range rra {
		ttl := rr.Header().Ttl
		if ttl > x {
			x = ttl
		}
	}
	return x
}

// Used by signResponseSection.
func (tx *stx) signRRs(rra []dns.RR, useKSK bool) (dns.RR, error) {
	if len(rra) == 0 {
		return nil, fmt.Errorf("no RRs to such")
	}

	maxttl := rraMaxTTL(rra)
	exp := time.Duration(maxttl)*time.Second + time.Duration(10)*time.Minute

	log.Info("maxttl: ", maxttl, "  expiration: ", exp)

	now := time.Now()
	rrsig := &dns.RRSIG{
		Hdr:        dns.RR_Header{Ttl: maxttl},
		Algorithm:  dns.RSASHA256,
		Expiration: uint32(now.Add(exp).Unix()),
		Inception:  uint32(now.Add(time.Duration(-10) * time.Minute).Unix()),
		SignerName: dns.Fqdn(tx.soa.Hdr.Name),
	}
	pk := tx.e.cfg.ZSKPrivate
	if useKSK {
		pk = tx.e.cfg.KSKPrivate
		rrsig.KeyTag = tx.e.cfg.KSK.KeyTag()
	} else {
		rrsig.KeyTag = tx.e.cfg.ZSK.KeyTag()
	}

	err := rrsig.Sign(pk.(crypto.Signer), rra)
	if err != nil {
		return nil, err
	}

	return rrsig, nil
}

// Used by signResponse.
func (tx *stx) signResponseSection(rra *[]dns.RR) error {
	if len(*rra) == 0 {
		return nil
	}
	//log.Info("sign section: ", *rra)

	i := 0
	a := []dns.RR{}
	pt := (*rra)[0].Header().Rrtype
	t := uint16(0)

	origrra := *rra

	for i < len(origrra) {
		for i < len(origrra) {
			t = (*rra)[i].Header().Rrtype
			if t != pt {
				break
			}

			a = append(a, origrra[i])
			i++
		}

		if shouldSignType(pt, (rra == &tx.res.Ns)) {
			useKSK := (pt == dns.TypeDNSKEY && tx.e.cfg.KSK != nil)
			if useKSK {
				srr, err := tx.signRRs(a, true)
				if err != nil {
					return err
				}

				*rra = append(*rra, srr)
			}

			srr, err := tx.signRRs(a, false)
			if err != nil {
				return err
			}

			*rra = append(*rra, srr)
		}

		pt = t
		a = []dns.RR{}
	}

	return nil
}

// This is called to append RRSIGs to the response based on the current records in the Answer and
// Authority sections of the response. Records in the Additional section are not signed.
func (tx *stx) signResponse() error {
	if !tx.useDNSSEC() {
		return nil
	}

	for _, r := range []*[]dns.RR{&tx.res.Answer, &tx.res.Ns /*&tx.res.Extra*/} {
		err := tx.signResponseSection(r)
		if err != nil {
			log.Infoe(err, "fail signResponse")
			return err
		}
	}

	log.Info("done signResponse")
	return nil
}

// Used for sorting RRTYPE lists for encoding into type bit maps.
type uint16Slice []uint16

func (p uint16Slice) Len() int           { return len(p) }
func (p uint16Slice) Less(i, j int) bool { return p[i] < p[j] }
func (p uint16Slice) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }

// © 2014 Hugo Landau <hlandau@devever.net>    GPLv3 or later
