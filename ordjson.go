package main

// JSON walked in document order, the way jq's `..` walks it: a value, then
// its children, object keys in the order they were written. A Go map would
// lose that order, and the order decides which text comes first in a Jira
// description and which workspace_id a hook event names first.

import (
	"bytes"
	"encoding/json"
)

type jnode struct {
	val   any      // the scalar (string, json.Number, bool, nil) when not a container
	keys  []string // object keys, in document order
	vals  []*jnode // object values, parallel to keys
	items []*jnode // array items
	isObj bool
	isArr bool
}

// parseOrdered decodes data into a jnode tree, or nil when it is not JSON.
func parseOrdered(data []byte) *jnode {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := decodeNode(dec)
	if err != nil {
		return nil
	}
	return n
}

func decodeNode(dec *json.Decoder) (*jnode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &jnode{isObj: true}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, kt.(string))
				n.vals = append(n.vals, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		case '[':
			n := &jnode{isArr: true}
			for dec.More() {
				v, err := decodeNode(dec)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return n, nil
		}
	}
	return &jnode{val: tok}, nil
}

// get is the value under key k of an object node, nil when absent.
func (n *jnode) get(k string) *jnode {
	if n == nil || !n.isObj {
		return nil
	}
	for i, key := range n.keys {
		if key == k {
			return n.vals[i]
		}
	}
	return nil
}

// preorder calls visit on n and every node under it, in jq's `..` order.
func (n *jnode) preorder(visit func(*jnode)) {
	if n == nil {
		return
	}
	visit(n)
	for _, v := range n.vals {
		v.preorder(visit)
	}
	for _, v := range n.items {
		v.preorder(visit)
	}
}

// stringsUnder is jq's `[.. | .key? // empty]` restricted to strings: the
// string values under key in every object, in document order.
func (n *jnode) stringsUnder(key string) []string {
	var out []string
	n.preorder(func(m *jnode) {
		if v := m.get(key); v != nil {
			if s, ok := v.val.(string); ok {
				out = append(out, s)
			}
		}
	})
	return out
}
