---
name: Bug report
about: Something latem does that it should not
labels: bug
---

## What happened

<!-- The command you ran and what came back. Verbatim beats paraphrase: latem's
     error codes are specific, and the code is usually the fastest route to the
     cause. -->

```console
$ latem …
```

## What you expected instead

## The machine

<!-- `latem version` names the release and the revision it was built from, and
     `latem doctor` is the fastest way to rule out an environment problem —
     it checks thirty-odd things and names the fix for each. Paste both;
     neither prints a secret value. -->

```console
$ latem version

$ latem doctor
```

**Executor:** native / docker / tart
**Job registry:** the `.latem/jobs.yaml` job that failed, if the failure needs one

## Anything you already ruled out

<!-- Optional, and the most useful box on this form when it is filled in. -->
