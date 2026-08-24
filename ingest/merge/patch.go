// SPDX-FileCopyrightText: 2026 NOI Techpark <digital@noi.bz.it>
//
// SPDX-License-Identifier: MPL-2.0

package merge

// MergePatch applies an RFC 7386 JSON Merge Patch to target, in place, and
// returns the result.
//
// The rules are the whole of the spec:
//
//   - a patch that is not an object replaces the target outright;
//   - a null member removes that key;
//   - an object member merges recursively;
//   - anything else replaces that key.
//
// Recursive merging is why a subtree rule is safe by default. Patching
// `{"name_it":"x"}` onto `{"provider_id":"…","capacity":245}` yields all three
// keys — a provider-written key under the destination survives unless the patch
// names it. That is the difference between this and a wholesale assignment, and
// it is the reason a rule may target a subtree the provider also writes into.
//
// Arrays are replaced wholesale, per the spec. Anything needing per-element
// identity is outside what a rule can express.
func MergePatch(target any, patch any) any {
	p, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	t, ok := target.(map[string]any)
	if !ok {
		t = map[string]any{}
	}
	for k, v := range p {
		if v == nil {
			delete(t, k)
			continue
		}
		if sub, ok := v.(map[string]any); ok {
			t[k] = MergePatch(t[k], sub)
			continue
		}
		t[k] = v
	}
	return t
}
