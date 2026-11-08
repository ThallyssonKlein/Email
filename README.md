# email-tokens

Baixa **todos** os emails da sua conta Gmail (somente leitura) e calcula quantos
tokens uma IA consumiria para ler todos, com estimativa de custo.

- **OpenAI**: contagem local, offline e grátis com `tiktoken` (`o200k_base` = GPT-4o / 4.1 / 5).
- **Anthropic**: contagem exata pela API oficial `count_tokens` (gratuita, só tem rate limit).
  Vários emails vão em cada requisição, então são poucas chamadas.

> O `tiktoken` é o tokenizer da OpenAI e conta **menos** tokens do que o Claude usa de verdade.
> Para o Claude, use o número da API da Anthropic.

## 1. Credenciais do Google (uma vez só)

1. Acesse <https://console.cloud.google.com/> e crie um projeto (ou use um existente).
2. **APIs e serviços → Biblioteca** → ative a **Gmail API**.
3. **APIs e serviços → Tela de consentimento OAuth** → tipo *Externo* → adicione
   seu email em **Usuários de teste**.
4. **APIs e serviços → Credenciais → Criar credenciais → ID do cliente OAuth** →
   tipo **App para computador (Desktop app)**.
5. Baixe o JSON e salve como `credentials.json` nesta pasta.

Na primeira execução o programa mostra um link: abra, autorize, e o token fica
salvo em `token.json`. Esse arquivo e o `credentials.json` já estão no `.gitignore`.

## 2. Chave da Anthropic

Copie o exemplo e coloque sua chave:

```bash
cp .env.example .env
# edite o .env: ANTHROPIC_API_KEY=sk-ant-...
```

O `.env` já está no `.gitignore`. Também funciona com `export ANTHROPIC_API_KEY=...`
no terminal, e nesse caso a variável do terminal tem prioridade. Para usar outro
arquivo, passe `-env caminho/arquivo`.

Crie a chave em <https://platform.claude.com/>. Contar tokens não custa nada.
Se não quiser usar, passe `-no-anthropic`.

## 3. Rodar

```bash
go build -o email-tokens .

./email-tokens                                   # baixa tudo e conta
./email-tokens -max 500                          # teste rápido com 500 emails
./email-tokens -q "after:2024/01/01 -category:promotions"
./email-tokens -skip-fetch                       # só reconta o que já foi baixado
./email-tokens -skip-fetch -openai-price 2.50    # mostra também a estimativa de custo da OpenAI
./email-tokens -models claude-sonnet-5-5         # escolher modelos Claude
```

### Sobre os valores em dólar

**Rodar este script é grátis.** Os valores em USD do relatório são uma simulação:
quanto você *pagaria* se mandasse todos os emails para a IA ler.

- **Claude:** os preços já estão no código, então a estimativa aparece automaticamente.
- **OpenAI:** os preços não estão no código. Consulte o preço de **input** (em USD por
  1 milhão de tokens) do modelo que quiser em <https://openai.com/api/pricing/> e passe
  em `-openai-price`. Sem essa opção, o relatório mostra só a quantidade de tokens.
  O `2.50` acima é só um exemplo.

O download fica em `emails.jsonl`. Se cair ou você apertar Ctrl+C, rode de novo:
ele continua de onde parou.

A cota padrão do Gmail é de 6.000 unidades por minuto, e cada email custa 5 unidades,
o que dá no máximo 20 emails/s. O script baixa 15/s por padrão (mude com `-rate`).
Nesse ritmo, 20 mil emails levam ~25 min e 100 mil ~2 h. Se a cota estourar mesmo
assim, o script pausa todos os downloads até a cota renovar e continua sozinho.

## O que é contado

Para cada email: `From`, `To`, `Cc`, `Date`, `Subject`, nomes dos anexos e o corpo.

- Se o email tem uma parte `text/plain`, usa ela.
- Se só tem HTML, extrai o texto visível (sem tags, CSS, scripts). Mandar HTML cru
  gastaria muito mais tokens.
- **Anexos não são lidos**, só os nomes entram na conta.
- Spam e Lixeira ficam de fora (use `-spam` para incluir).

O custo mostrado é só de **input** (leitura). Se for pedir uma resposta para cada
email, some os tokens de saída e o prompt de instrução repetido em cada chamada.
