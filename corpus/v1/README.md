# Attribution matcher corpus v1

`matcher_corpus.json` defines portable test cases for matching ordered
tool-produced lines to final added lines. Each case records the input lines and
the expected match tier for every added line.

Implementations can vendor the JSON and pin it by release, commit, or checksum.
All implementations must return the same tiers for these cases. Changes to the
tier vocabulary or expected results require a new corpus version.

## Schema

```json
{
  "version": 1,
  "tiers": ["none", "exact", "normalized"],
  "cases": [
    {
      "name": "unique-case-name",
      "description": "what the case checks",
      "claims": ["ordered claimed lines"],
      "added":  ["ordered final added lines"],
      "expected": ["tier per added line, parallel to `added`"]
    }
  ]
}
```

## Tiers

- `exact` - the added line equals a claimed line after trimming leading/trailing
  whitespace.
- `normalized` - the lines match only after removing all whitespace. These
  matches fill gaps between exact anchors.
- `none` - no claim aligns to the added line. Blank lines never match.

Claims are matched to added lines in order (each claim to at most one added
line), so duplicates and reorderings are resolved by occurrence, not by set
membership.

## Running

Run the corpus against the CLI scorer with:

```sh
go test ./corpus/v1/
```
