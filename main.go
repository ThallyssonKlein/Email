// email-tokens: baixa todos os emails de uma conta Gmail e estima quantos
// tokens uma IA (Anthropic Claude / OpenAI GPT) consumiria para ler todos.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"

	"github.com/joho/godotenv"
)

type config struct {
	envFile      string
	credentials  string
	tokenFile    string
	cache        string
	query        string
	max          int
	includeSpam  bool
	skipFetch    bool
	workers      int
	models       string
	noAnthropic  bool
	openaiPrice  float64
	openaiEncode string
}

func main() {
	var c config
	flag.StringVar(&c.envFile, "env", ".env", "arquivo .env com ANTHROPIC_API_KEY (opcional)")
	flag.StringVar(&c.credentials, "credentials", "credentials.json", "arquivo OAuth client (Desktop app) baixado do Google Cloud Console")
	flag.StringVar(&c.tokenFile, "token", "token.json", "onde salvar/ler o token OAuth do usuário")
	flag.StringVar(&c.cache, "cache", "emails.jsonl", "cache local dos emails baixados (permite retomar)")
	flag.StringVar(&c.query, "q", "", `filtro de busca do Gmail, ex: "after:2024/01/01 -category:promotions"`)
	flag.IntVar(&c.max, "max", 0, "limite de emails a baixar (0 = todos)")
	flag.BoolVar(&c.includeSpam, "spam", false, "incluir Spam e Lixeira")
	flag.BoolVar(&c.skipFetch, "skip-fetch", false, "não baixar nada, só contar o que já está no cache")
	flag.IntVar(&c.workers, "workers", 10, "downloads simultâneos do Gmail")
	flag.StringVar(&c.models, "models", "claude-opus-5-5,claude-sonnet-5-5,claude-haiku-5-5", "modelos Claude para contar tokens (separados por vírgula)")
	flag.BoolVar(&c.noAnthropic, "no-anthropic", false, "pular a contagem via API da Anthropic")
	flag.Float64Var(&c.openaiPrice, "openai-price", 0, "preço de input da OpenAI em USD por 1M tokens, só para o relatório calcular a estimativa (não gera cobrança)")
	flag.StringVar(&c.openaiEncode, "openai-encoding", "o200k_base", "encoding do tiktoken (o200k_base = GPT-4o/4.1/5; cl100k_base = GPT-4/3.5)")
	flag.Parse()

	// Carrega o .env se existir. Variáveis já exportadas no terminal têm prioridade.
	if err := godotenv.Load(c.envFile); err == nil {
		fmt.Printf("Variáveis carregadas de %s\n", c.envFile)
	} else if !os.IsNotExist(err) {
		log.Fatalf("erro lendo %s: %v", c.envFile, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if !c.skipFetch {
		if err := fetchAll(ctx, c); err != nil {
			log.Fatalf("erro baixando emails: %v", err)
		}
	}

	emails, err := loadCache(c.cache)
	if err != nil {
		log.Fatalf("erro lendo cache: %v", err)
	}
	if len(emails) == 0 {
		log.Fatal("nenhum email no cache")
	}
	fmt.Printf("\n%d emails no cache. Contando tokens...\n", len(emails))

	report := &report{emails: len(emails)}
	for _, e := range emails {
		report.chars += int64(len([]rune(e.Text())))
		if e.HTMLOnly {
			report.htmlOnly++
		}
		report.attachments += len(e.Attachments)
	}

	if err := countOpenAI(emails, c.openaiEncode, report); err != nil {
		log.Printf("aviso: contagem OpenAI falhou: %v", err)
	}

	if !c.noAnthropic && os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" {
		fmt.Println("  (ANTHROPIC_API_KEY não definida; tentando o perfil do `ant auth login`)")
	}
	if !c.noAnthropic {
		for _, m := range strings.Split(c.models, ",") {
			m = strings.TrimSpace(m)
			if m == "" {
				continue
			}
			n, err := countAnthropic(ctx, emails, m)
			if err != nil {
				log.Printf("aviso: contagem Anthropic (%s) falhou: %v", m, err)
				continue
			}
			report.claude = append(report.claude, modelCount{model: m, tokens: n})
		}
	}

	report.print(c)
}
