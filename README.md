# email-tokens

Downloads **all** the emails from your Gmail account (read-only) and calculates how
many tokens an AI would consume to read them, with a cost estimate.

- **OpenAI**: local, offline, free count with `tiktoken` (`o200k_base` = GPT-4o / 4.1 / 5).
- **Anthropic**: exact count through the official `count_tokens` API (free, only rate limited).
  Several emails go in each request, so there are few calls.

> `tiktoken` is OpenAI's tokenizer and counts **fewer** tokens than Claude actually uses.
> For Claude, use the number from the Anthropic API.

## 1. Google credentials (one time only)

1. Go to <https://console.cloud.google.com/> and create a project (or use an existing one).
2. **APIs & Services → Library** → enable the **Gmail API**.
3. **APIs & Services → OAuth consent screen** → type *External* → add
   your email under **Test users**.
4. **APIs & Services → Credentials → Create credentials → OAuth client ID** →
   type **Desktop app**.
5. Download the JSON and save it as `credentials.json` in this folder.

On the first run the program shows a link: open it, authorize, and the token is
saved in `token.json`. That file and `credentials.json` are already in `.gitignore`.

## 2. Anthropic key

Copy the example and put your key in it:

```bash
cp .env.example .env
# edit .env: ANTHROPIC_API_KEY=sk-ant-...
```

`.env` is already in `.gitignore`. It also works with `export ANTHROPIC_API_KEY=...`
in the terminal, in which case the terminal variable takes priority. To use another
file, pass `-env path/to/file`.

Create the key at <https://platform.claude.com/>. Counting tokens costs nothing.
If you don't want to use it, pass `-no-anthropic`.

## 3. Run

```bash
go build -o email-tokens .

./email-tokens                                   # downloads everything and counts
./email-tokens -max 500                          # quick test with 500 emails
./email-tokens -q "after:2024/01/01 -category:promotions"
./email-tokens -skip-fetch                       # only recounts what was already downloaded
./email-tokens -skip-fetch -openai-price 2.50    # also shows the OpenAI cost estimate
./email-tokens -models claude-sonnet-5-5         # choose Claude models
```

### About the dollar amounts

**Running this script is free.** The USD amounts in the report are a simulation:
what you *would pay* if you sent all the emails to the AI to read.

- **Claude:** the prices are already in the code, so the estimate appears automatically.
- **OpenAI:** the prices are not in the code. Look up the **input** price (in USD per
  1 million tokens) of the model you want at <https://openai.com/api/pricing/> and pass
  it in `-openai-price`. Without this option, the report shows only the token count.
  The `2.50` above is just an example.

The download is saved in `emails.jsonl`. If it drops or you press Ctrl+C, run it again:
it continues where it stopped.

The default Gmail quota is 6,000 units per minute, and each email costs 5 units,
which gives at most 20 emails/s. The script downloads 15/s by default (change it with `-rate`).
At that pace, 20 thousand emails take ~25 min and 100 thousand ~2 h. If the quota is
exhausted anyway, the script pauses all downloads until the quota renews and continues on its own.

## What is counted

For each email: `From`, `To`, `Cc`, `Date`, `Subject`, attachment names and the body.

- If the email has a `text/plain` part, it uses that.
- If it only has HTML, it extracts the visible text (no tags, CSS, scripts). Sending raw
  HTML would spend far more tokens.
- **Attachments are not read**, only their names are counted.
- Spam and Trash are left out (use `-spam` to include them).

The cost shown covers **input** (reading) only. If you ask for a reply to each
email, add the output tokens and the instruction prompt repeated in each call.
