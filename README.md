<h1 align="center">reddit</h1>

<p align="center">
  <a href="LICENSE"><img src="https://img.shields.io/badge/LICENSE-MIT-5C9E31?style=for-the-badge" alt="License"></a>
  <a href="https://github.com/jgalea"><img src="https://img.shields.io/badge/BUILT%20BY-JGALEA-8A2BE2?style=for-the-badge&logo=github&logoColor=white" alt="Built by jgalea"></a>
</p>

<p align="center"><strong>Search and read Reddit threads from the terminal, through your own signed-in Chrome profile.</strong></p>

---

Reddit answers plain HTTP requests, `.json` endpoints and headless browsers with a block page. `reddit` loads the real pages in a normal Chrome window that uses its own profile, signed in by you, and prints the rendered text. It reads only: it never posts, votes or messages.

## Install

```
go install github.com/jgalea/reddit@latest
```

Needs Go and a local Chrome or Chromium. On macOS the browser starts hidden and focus goes straight back to the app you were using. Elsewhere it opens a visible window.

## Use

```
reddit login                      sign in by hand, once
reddit status                     whether the saved profile holds a Reddit session
reddit search QUERY [--sub NAME] [--sort relevance|new|top|comments] [--time hour|day|week|month|year|all] [--limit N]
reddit thread URL                 a post with its comments
reddit sub NAME [--sort hot|new|top] [--time ...]
reddit page URL                   the text of any reddit.com page
reddit research QUERY [--also "q2|q3"] [--sub NAME] [--threads N] [--terms a,b,c]
                                  search, read the top threads in one pass, and tally how many threads mention each --terms name
reddit sentiment NAME [--also "q2|q3"] [--sub NAME] [--threads N]
                                  the comment lines about one product or brand, split into positive and negative by cue words
```

`--max N` caps the characters printed (default 20000). `--show` opens a visible window.

The profile and a copy of the session cookies live in `~/.reddit` (set `REDDIT_HOME` to move it, `REDDIT_CHROME` to pick the browser binary). The cookie file is created with mode 0600.
