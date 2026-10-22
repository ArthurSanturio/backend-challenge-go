# Desafio Backend — Processamento Distribuído de Apostas em Go

Implemente um serviço em **Go**, com **Uber Fx**, para processar operações financeiras de provedores de jogos em um ambiente distribuído.

## 1. Objetivo

A aplicação deve oferecer uma API HTTP e um consumidor de mensagens que movimentem carteiras de jogadores com garantias equivalentes. Demonstre que o resultado financeiro continua correto com várias instâncias em execução e falhas entre as etapas do processamento.

A avaliação considera precisão monetária, integridade do ledger, idempotência persistente, concorrência, recuperação de falhas e decisões de arquitetura.

## 2. Autenticação e autorização

Autenticação e autorização são **obrigatórias**, com integração a um IdP externo OAuth 2.0/OIDC.

Recomenda-se **Keycloak** no Docker Compose e `client_credentials` para comunicação entre serviços. A escolha do IdP, a validação de credenciais e o modelo de permissões devem ser justificados em `ARCHITECTURE.md`. Cadastro de senhas e emissão própria de tokens estão fora do escopo.

A identidade autenticada deve determinar o `providerId` autorizado. Provedores acessam apenas suas próprias transações, inclusive em replays; operações de carteira são restritas ao serviço interno.

O acesso à mensageria deve ser controlado por credenciais e políticas do broker, preservando as validações de domínio no consumidor.

## 3. Ambiente de execução e falhas

Cada operação externa pertence a um jogador, uma carteira, um jogo, um provedor e uma rodada. Os tipos externos são `BET`, `WIN`, `LOSS`, `REFUND` e `ROLLBACK`.

Assuma entrega **at-least-once** e prepare a solução para:

- recebimento repetido de uma mesma operação, inclusive por HTTP e SQS;
- chegada de uma reversão antes da transação que ela referencia;
- processamento simultâneo de operações da mesma carteira;
- encerramento abrupto antes ou depois de um commit;
- publicação repetida de um evento de integração;
- indisponibilidade temporária do PostgreSQL ou do SQS.

Nenhuma dessas situações pode gerar movimentação duplicada, saldo negativo ou perda de um evento cujo registro foi confirmado no banco.

## 4. Stack

### Tecnologias obrigatórias

| Responsabilidade | Tecnologia |
| --- | --- |
| Linguagem e compilação | Go; declare a versão utilizada em `go.mod` e no Dockerfile |
| Dependências | Go Modules, com `go.mod` e `go.sum` versionados |
| Composição da aplicação | Uber Fx (`go.uber.org/fx`) |
| HTTP | `net/http` ou um roteador Go à sua escolha |
| Autenticação | IdP externo OAuth 2.0/OIDC; Keycloak recomendado |
| Persistência | PostgreSQL |
| Mensageria | AWS SQS, executado localmente com LocalStack ou MiniStack |
| Ambiente local | Docker Compose |
| Evolução do banco | Migrations versionadas, com aplicação e reversão documentadas |
| Testes | `testing` e `go test`, incluindo execução com `-race` |

### Acesso ao banco

`pgx` com SQL explícito é preferencial; `sqlc` é opcional. `database/sql` e GORM são aceitos. Transações, locks e constraints devem permanecer explícitos e verificáveis.

Documente em `ARCHITECTURE.md` a biblioteca escolhida, o mapeamento de `Money` e a delimitação da transação SQL entre os repositórios.

### Composição e ciclo de vida

Use Uber Fx na composição de configuração, conexões, repositórios, casos de uso, handlers e workers, com injeção por construtores e organização por `fx.Module`, `fx.Provide` e `fx.Invoke`.

Gerencie servidor, workers e recursos com `fx.Lifecycle`:

- inicialização com validação de configuração e dependências;
- cancelamento, prazos de execução e término observável dos workers;
- shutdown com interrupção de novas entradas e conclusão ou liberação do trabalho em andamento;
- fechamento das dependências após a finalização dos componentes que as utilizam.

O domínio deve permanecer independente de Fx, HTTP, SQS e bibliotecas de persistência. A organização dos pacotes fica a critério do candidato.

## 5. Garantias obrigatórias

1. Dinheiro não pode passar por `float32` ou `float64`, nem durante parsing, cálculo, serialização ou persistência.
2. Idempotência deve ser persistente e sobreviver ao reinício de todos os processos.
3. As invariantes financeiras devem ser garantidas no banco, independentemente de locks locais e da deduplicação do SQS FIFO.
4. Eventos externos só podem ser publicados depois da confirmação da transação que os originou.
5. O ledger deve ser append-only: correções financeiras exigem novos lançamentos.
6. Carteiras independentes devem avançar em paralelo; locks globais são proibidos.
7. Atualizações de saldo devem impedir lost updates.
8. Unicidade, não negatividade e imutabilidade do ledger devem ser impostas pelo schema, pelas constraints e pelos mecanismos de proteção do banco.

## 6. Modelo de domínio

### Encapsulamento e erros

Modele entidades com estado encapsulado, construtores com validação e métodos explícitos de transição. As invariantes devem ser preservadas em todas as operações públicas.

Separe criação e reidratação. A reidratação não deve reaplicar movimentações, transições ou emissão de eventos.

Valores de domínio não inicializados ou inválidos devem ser rejeitados.

Erros de domínio devem ser classificáveis por tipo ou `errors.Is`/`errors.As`. `panic` não deve representar rejeições de negócio. Operações de I/O devem receber `context.Context` e respeitar cancelamento e timeout.

### 6.1. Money

`Money` é um value object imutável, com valor e moeda. Deve suportar criação a partir de string decimal, zero por moeda, soma, subtração, negação, comparação e serialização.

Use `int64` em unidades mínimas ou uma biblioteca decimal de precisão exata. Documente a representação e seus limites.

- O contrato externo recebe e devolve valores como `{"amount":"25.00","currency":"BRL"}`.
- Use escala fixa de duas casas e código de moeda ISO 4217.
- Rejeite valores vazios, `NaN`, `Infinity`, notação científica, escala excedente e valores negativos nas entradas financeiras externas.
- Não arredonde silenciosamente uma entrada inválida. Caso aceite formas equivalentes, documente a normalização anterior ao hash de idempotência.
- Aritmética e comparação de valores monetários exigem moedas compatíveis.
- Se utilizar `int64`, trate overflow no parsing, na soma, na subtração e na negação.
- Valores negativos são permitidos em diferenças e cálculos internos, mas não no saldo da carteira.
- A persistência deve preservar exatamente valor e moeda, por exemplo com unidades mínimas em `BIGINT` ou decimal em `NUMERIC`.

É permitido operar apenas em BRL nos cenários principais, desde que o tipo carregue a moeda e existam testes de incompatibilidade entre moedas.

### 6.2. Wallet

A carteira é a raiz do agregado financeiro. Deve carregar identidade, jogador, moeda, saldo, versão e instantes de criação e atualização.

Exponha criação, reidratação e operações de débito/crédito, mantendo a alteração do saldo sob controle do agregado e da transação SQL.

- O par `(playerId, currency)` identifica uma única carteira.
- Débitos precisam preservar saldo maior ou igual a zero.
- A moeda de cada movimentação deve coincidir com a da carteira.
- Cada mudança financeira exige o lançamento correspondente no ledger, confirmado junto com o saldo.
- A versão inicial é `1`; depois da criação, incremente-a apenas quando houver mudança de saldo.
- Disputas entre escritores não podem descartar uma atualização confirmada.

A estratégia de controle de concorrência deve ser documentada.

### 6.3. WagerTransaction

Tipos: `OPENING`, `BET`, `WIN`, `LOSS`, `REFUND` e `ROLLBACK`.

Para operações externas, a transação registra os identificadores interno e externo, provedor, chave de idempotência, hash do payload, carteira, jogador, rodada, jogo, tipo, `Money`, referência externa opcional, estado e timestamps. Quando aplicável, persista também a referência interna resolvida, o código de falha e o resultado financeiro retornado ao provedor.

A transação inicia em `PENDING`. As transições para processamento, espera por referência, rejeição e falha permanente devem ser validadas pelo domínio.

| Estado | Significado |
| --- | --- |
| `PENDING` | Registro aceito, com processamento ainda não concluído |
| `PENDING_REFERENCE` | A aplicação depende de uma referência ainda indisponível |
| `PROCESSED` | Operação concluída com sucesso; estado terminal |
| `REJECTED` | Operação recusada por uma regra de negócio; estado terminal |
| `FAILED` | Falha permanente de infraestrutura registrada para auditoria; estado terminal |

Uma transação terminal não deve sofrer novas transições. Replay consulta seu resultado persistido sem reaplicar a operação. Documente a máquina de estados e como distingue falhas transitórias de falhas permanentes.

Todo `PENDING` confirmado deve ter retomada durável por outra instância após uma interrupção. Operações sem dependências podem ser concluídas de forma síncrona, sem commit intermediário de aceite.

`OPENING` é reservado à abertura interna de carteira. Rejeite esse tipo quando enviado por HTTP ou SQS.

`OPENING` exige identidade interna estável, carteira, jogador, moeda, valor, estado e timestamps. Provedor, ID externo, chave e hash externos, rodada, jogo e referência não se aplicam a essa origem. O schema deve distinguir operações internas e externas e impedir crédito inicial duplicado.

### 6.4. WalletLedgerEntry

Cada lançamento registra `id`, `walletId`, `transactionId`, direção (`DEBIT` ou `CREDIT`), valor, saldo anterior, saldo posterior e instante de criação.

O lançamento é imutável e sua construção deve validar `balanceAfter = balanceBefore ± money`, conforme a direção.

Imponha no banco a unicidade de `(walletId, transactionId)` e a proteção contra edição ou exclusão. `LOSS` e operações rejeitadas não produzem lançamentos. Um ledger de partidas dobradas é opcional.

### 6.5. Inbox e outbox

| Registro | Informações e comportamento esperados |
| --- | --- |
| Inbox | Identidade da mensagem e do consumidor, hash, recebimento e conclusão; unicidade de `(consumerName, messageId)` |
| Outbox | Identidade estável do evento, agregado, tipo, payload, ocorrência, tentativas, próximo envio e publicação; suporte a retry com backoff |

Na entrada por SQS, o registro da inbox e a conclusão durável do tratamento devem compartilhar a transação SQL das alterações de domínio, do ledger e dos eventos correspondentes. Uma referência pendente pode ter sua mensagem de entrada concluída após a pendência estar persistida; o worker de referências assume a continuidade.

## 7. Operações e referências

| Tipo | Movimentação | Condição |
| --- | --- | --- |
| `BET` | Débito | Exige valor positivo e saldo suficiente |
| `WIN` | Crédito | Exige valor positivo; pode informar uma aposta da mesma rodada como referência |
| `LOSS` | Sem movimentação | Exige `money.amount` igual a `"0.00"`; não cria ledger nem altera a versão da carteira |
| `REFUND` | Crédito | Devolve integralmente o valor de uma `BET` processada |
| `ROLLBACK` | Movimento contrário ao original | Desfaz integralmente uma `BET`, `WIN` ou `REFUND` processada |

Para `REFUND` e `ROLLBACK`, `referenceExternalTransactionId` é obrigatório e deve ser resolvido por `(providerId, referenceExternalTransactionId)`.

A operação e sua referência devem concordar em provedor, jogador, carteira, moeda e rodada. O valor da reversão precisa ser igual ao valor referenciado; reversões parciais não fazem parte do desafio.

Para este desafio, zero é aceito no saldo inicial e em `LOSS`; `BET`, `WIN`, `REFUND` e `ROLLBACK` exigem valor maior que zero. `LOSS` continua exigindo a moeda da carteira e, quando processado, produz `WagerTransactionProcessed`, sem `WalletBalanceChanged`.

Garanta que uma referência não receba duas reversões bem-sucedidas do mesmo tipo. Documente como trata combinações de `REFUND` e `ROLLBACK` sobre a mesma aposta, preservando a coerência financeira e impedindo devolução duplicada do mesmo débito.

Uma reversão que precisaria debitar mais que o saldo disponível deve ser rejeitada e auditável. Seu código de falha deve ser diferente daquele usado para uma aposta sem saldo.

### Referências ainda indisponíveis

Persista a operação como `PENDING_REFERENCE` quando a referência ainda não tiver chegado. Um worker deve tentar novamente com backoff exponencial, inclusive após reinicialização da aplicação.

Defina um número máximo de tentativas ou TTL. Quando esgotado, finalize como `REJECTED`, informando um código de referência não encontrada e produzindo o evento de rejeição. Explique também o comportamento quando a referência existe, mas ainda está pendente ou terminou sem sucesso.

Toda rejeição deve fornecer um `failureCode` estável e documentado, distinguindo entradas corrigíveis de resultados definitivos.

## 8. Concorrência

A coordenação deve ocorrer por carteira. Escolha locking pessimista, controle otimista com retry limitado, atualização atômica condicionada ou uma combinação justificável.

As garantias devem ser demonstradas com pelo menos três processos independentes, cada um com suas próprias conexões e memória.

Teste obrigatório: uma carteira com **100.00 BRL** recebe, ao mesmo tempo, duas apostas distintas de **80.00 BRL**.

O resultado deve conter uma aposta processada, uma rejeição por saldo insuficiente, saldo final de **20.00 BRL** e um único débito no ledger. Reenvios não podem alterar esse resultado. Carteiras diferentes devem continuar sendo processadas em paralelo.

## 9. Contratos HTTP

### Abertura de carteira

```http
POST /wallets
Content-Type: application/json
```

```json
{
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "initialBalance": { "amount": "1000.00", "currency": "BRL" }
}
```

Exemplo de resposta:

```json
{
  "id": "0192f291-27dd-7d3f-8071-5f8685deef37",
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "balance": { "amount": "1000.00", "currency": "BRL" },
  "version": 1
}
```

Uma abertura com saldo positivo deve criar `OPENING` em `PROCESSED`, seu lançamento de crédito e os registros de outbox para `WagerTransactionProcessed` e `WalletBalanceChanged` no mesmo commit da carteira. Esses eventos de origem interna não exigem os metadados externos inaplicáveis; a versão da carteira nessa abertura é `1`. Saldo inicial zero não cria `OPENING`, ledger nem esses eventos financeiros. Tentar abrir outra carteira para o mesmo jogador e moeda deve resultar em conflito.

### Leitura

```http
GET /wallets/:walletId
GET /wallets/:walletId/ledger?cursor=...&limit=50
GET /wagering/transactions/:transactionId
GET /providers/:providerId/wagering/transactions/:externalTransactionId
```

A paginação do ledger deve usar cursor opaco e ordenação estável. As consultas de transação devem permitir acompanhar pendências e consultar códigos de rejeição ou falha.

### Envio de operação

```http
POST /wagering/transactions
Content-Type: application/json
Idempotency-Key: provider-a:transaction-123
```

```json
{
  "providerId": "provider-a",
  "externalTransactionId": "transaction-123",
  "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
  "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
  "roundId": "round-987",
  "gameId": "fortune-chimp",
  "kind": "BET",
  "money": { "amount": "25.00", "currency": "BRL" }
}
```

Exemplo após processamento:

```json
{
  "transactionId": "0192f298-345e-7e38-af88-e43f851a819d",
  "status": "PROCESSED",
  "balance": { "amount": "975.00", "currency": "BRL" },
  "idempotentReplay": false
}
```

Para reversões, acrescente `referenceExternalTransactionId` ao corpo.

O header `Idempotency-Key` é obrigatório. O cliente pode construí-lo como `{providerId}:{externalTransactionId}`, mas o servidor não deve substituir silenciosamente uma chave recebida por outra calculada.

Persista um hash determinístico dos campos de negócio, usando JSON canônico com ordenação de chaves. Exclua a chave de idempotência e os metadados de transporte desse cálculo. Documente algoritmo, campos e normalizações, garantindo equivalência entre HTTP e SQS.

- Chave e conteúdo equivalentes: retorne o resultado persistido, com `idempotentReplay: true`.
- Chave reutilizada com conteúdo diferente: devolva conflito.
- Uma operação financeira identificada por `(providerId, externalTransactionId)` não pode ser reaplicada usando outra chave.
- Para operações concluídas, o replay deve devolver o saldo observado no processamento original, mesmo que a carteira já tenha recebido outras movimentações.

Documente os códigos HTTP e os corpos de resposta para entrada inválida, conflito, rejeição de negócio, processamento pendente e indisponibilidade transitória. Essas situações precisam ser distinguíveis pelo contrato.

### Reconciliação

```http
POST /wallets/:walletId/reconciliation
```

Exemplo considerando apenas a abertura e a aposta apresentadas acima:

```json
{
  "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
  "storedBalance": { "amount": "975.00", "currency": "BRL" },
  "calculatedBalance": { "amount": "975.00", "currency": "BRL" },
  "difference": { "amount": "0.00", "currency": "BRL" },
  "consistent": true,
  "checkedEntries": 2
}
```

Reconstrua o saldo a partir do ledger, incluindo a abertura, e compare os valores em uma visão consistente dos dados. `difference` é o saldo armazenado menos o saldo reconstruído.

Reporte divergências na resposta, nos logs e em uma métrica. A reconciliação não deve alterar o saldo.

### Health checks públicos

```http
GET /health/live
GET /health/ready
```

Liveness do processo e readiness de PostgreSQL e SQS.

## 10. Consumidor SQS

Provisione as filas `wager-transactions.fifo` e `wager-transactions-dlq.fifo`, incluindo a configuração de redrive.

Exemplo de corpo de mensagem:

```json
{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}
```

HTTP e SQS devem compartilhar o caso de uso e as garantias de idempotência financeira. Na entrada por SQS, a chave é `data.idempotencyKey`, com deduplicação adicional pela inbox.

- Use o `messageId` do envelope como identidade durável da mensagem para o consumidor e verifique seu hash em reentregas.
- Remova a mensagem da fila somente após o commit do seu tratamento durável.
- Rejeições de negócio confirmadas são terminais e permitem a remoção da mensagem.
- Falhas transitórias exigem retry com backoff; erros permanentes ou tentativas esgotadas devem chegar à DLQ.
- Documente limites de tentativas, visibility timeout e tratamento de mensagens inválidas.
- Em `SIGTERM`, pare de buscar trabalho e conclua o processamento em andamento dentro do prazo, ou libere sua visibilidade para reentrega segura.

Documente `MessageGroupId` e `MessageDeduplicationId` e valide a concorrência entre entradas HTTP e SQS.

## 11. Publicação com transactional outbox

Estado da operação, saldo, ledger, inbox e registros de eventos devem ser confirmados atomicamente, conforme aplicável.

Um worker separado publica os registros pendentes da outbox. Ele deve suportar múltiplos publishers, disputa por registros, backoff e recuperação de trabalho abandonado.

Demonstre recuperação após interrupção entre commit e publicação e entre publicação e confirmação na outbox. Eventos pendentes devem ser assumidos por outra instância; republicações devem preservar o `eventId`.

Provisione o destino dos eventos de saída e documente seus contratos de roteamento e consumo.

### Eventos exigidos

| Evento | Gatilho |
| --- | --- |
| `WagerTransactionProcessed` | Conclusão bem-sucedida de uma operação, incluindo `LOSS` |
| `WagerTransactionRejected` | Rejeição definitiva por regra de negócio |
| `WalletBalanceChanged` | Alteração efetiva do saldo |
| `WagerTransactionPendingReference` | Registro de espera pela referência |

Defina tipos concretos por evento. O envelope deve conter `eventId`, `eventType`, `aggregateId`, `correlationId`, `causationId` opcional, `occurredAt`, `version` e `data` tipado.

O payload de `WalletBalanceChanged` deve incluir `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`, `balanceAfter` e `walletVersion`.

Tipo e versão devem ser definidos pelo construtor do evento. Use timestamps UTC em RFC 3339 e valores monetários em strings decimais. O payload da outbox deve ser um snapshot imutável.

## 12. Observabilidade

Produza logs JSON com os identificadores disponíveis para rastrear a operação: `correlationId`, `messageId`, `transactionId`, `walletId` e `providerId`. Não registre credenciais, dados sensíveis ou payloads financeiros completos.

Exponha métricas para resultados por status, duplicatas, retries, DLQ, conflitos de concorrência, atraso da outbox, latência de processamento e divergências de reconciliação.

Inclua os health checks definidos na API. Tracing com OpenTelemetry e dashboards são diferenciais opcionais.

## 13. Verificação obrigatória

### Testes unitários

Cubra parsing e operações de `Money`, escala, limites numéricos, entradas inválidas, incompatibilidade de moedas, invariantes da carteira, transições de estado, regras dos cinco tipos externos e conflito de payload para a mesma chave. Inclua a política de valores zero de cada tipo e a abertura interna com seus metadados e eventos.

### Testes de integração

Execute PostgreSQL, o IdP e LocalStack ou MiniStack em containers reais. Verifique migrations, constraints, imutabilidade do ledger, atomicidade financeira, inbox, reentrega, outbox concorrente, retry, DLQ e recuperação após reinicialização.

Adicione uma verificação da composição Fx e de seu início e encerramento, incluindo liberação de recursos dos workers. Não substitua toda a infraestrutura por mocks.

### Autenticação e autorização

- Integração real com o IdP e rejeição de credenciais ausentes, inválidas ou expiradas.
- Isolamento entre provedores, inclusive em consultas e replays, e restrição das operações internas.
- Ausência de efeitos financeiros ou exposição de dados em acessos não autorizados.

### Testes de concorrência e recuperação

1. Envie a mesma aposta 50 vezes em paralelo e comprove um único débito.
2. Execute a disputa das duas apostas de 80.00 sobre saldo de 100.00.
3. Processe carteiras distintas simultaneamente.
4. Repita cenários relevantes com pelo menos três instâncias independentes.
5. Interrompa um consumidor depois do commit e antes da remoção da mensagem; valide a reentrega.
6. Execute dois publishers disputando a mesma outbox e valide a recuperação de publicação.
7. Entregue `REFUND` ou `ROLLBACK` antes da referência e comprove a resolução posterior ou a rejeição por expiração.
8. Reinicie a aplicação e verifique que idempotência, pendências e consistência financeira foram preservadas. Se houver aceite assíncrono, interrompa o processo após confirmar `PENDING` e antes de executar a operação; outra instância deve retomá-la.

Ao final, confira o saldo armazenado contra a soma de créditos menos débitos do ledger. Inclua cenários que cruzem HTTP e SQS para a mesma operação.

Os testes de duplicidade devem exercitar a deduplicação da aplicação, com recebimentos repetidos comprovados.

Execute `go test -race` nos testes aplicáveis.

## 14. Critérios de avaliação

| Critério | Pontos | Evidência esperada |
| --- | ---: | --- |
| Integridade financeira | 20 | Precisão, invariantes, reversões e reconciliação confiáveis |
| Concorrência | 20 | Coordenação entre processos e ausência de atualizações perdidas |
| Idempotência | 15 | Persistência, detecção de conflito e reprodução do resultado original |
| Mensageria e recuperação | 15 | Inbox, outbox, retries, DLQ e encerramento seguro |
| Modelagem e arquitetura | 10 | Encapsulamento em Go, composição com Fx e políticas de autenticação e autorização |
| Testes | 10 | Integração real, isolamento entre provedores, paralelismo e interrupção |
| Observabilidade | 5 | Diagnóstico por logs, métricas e health checks |
| Documentação | 5 | Execução reproduzível e decisões técnicas explicadas |
| **Total** | **100** | |

São eliminatórios: ausência de autenticação efetiva nos endpoints de negócio, acesso não autorizado a operações ou transações, cálculo monetário em ponto flutuante, saldo negativo por concorrência, movimentação duplicada, idempotência restrita à memória, dependência de uma única instância para funcionar corretamente, publicação anterior ao commit, ausência de ledger auditável ou substituição integral de PostgreSQL, SQS e IdP por mocks nos testes.

Partidas dobradas, tracing e testes de carga são diferenciais opcionais. Testes de carga devem incluir comando reproduzível, ambiente, metodologia, throughput, p50/p95/p99, erros, conflitos e atraso da outbox. Não há meta mínima de RPS.

## 15. Entrega

Entregue o código, migrations, ambiente Docker Compose e instruções suficientes para outra pessoa reproduzir a solução a partir de um checkout limpo.

O `README.md` da solução deve explicar pré-requisitos, variáveis de ambiente, inicialização das filas, aplicação e reversão das migrations, execução da aplicação, exemplos de chamadas e comandos de teste. Inclua `.env.example` com valores locais de exemplo, sem segredos reais.

Inclua o provisionamento automático do IdP, identidades de teste e instruções para executar os fluxos autenticados.

No `ARCHITECTURE.md`, registre as decisões sobre dinheiro, transações, idempotência, locks, referências pendentes, reversões, inbox/outbox, autenticação, autorização, uso do Fx e shutdown. Explicite limitações, interpretações adotadas e trabalho não concluído.

Disponibilize os comandos abaixo ou equivalentes documentados:

```sh
docker compose up --build
go test ./...
go test -race ./...
go vet ./...
```

Documente separadamente como preparar as dependências dos testes e executar integração, múltiplas instâncias e simulações de falha. Se utilizar build tags, informe os comandos correspondentes.

Entregue código formatado com `gofmt` e dependências reproduzíveis.


---

# Guia de execução da solução

As seções anteriores preservam o enunciado do desafio. A partir daqui estão as instruções da implementação disponível neste projeto. As decisões técnicas e limitações estão em [ARCHITECTURE.md](ARCHITECTURE.md).

## 1. Pré-requisitos

- Docker com Docker Compose v2, com o daemon em execução.
- Go `1.27.2` para executar a aplicação fora do container e rodar testes. Para testes com `-race`, utilize uma instalação com suporte ao detector de corridas e compilador C disponível.
- `curl` e Python 3 para os exemplos HTTP.
- AWS CLI v2 para os exemplos opcionais de publicação SQS.
- Portas locais disponíveis: `8080` (API), `8081` (Keycloak), `5432` (PostgreSQL) e `4566` (MiniStack).

Execute os comandos na raiz do projeto. Os exemplos usam um shell compatível com Bash/Zsh e devem ser executados na mesma sessão para preservar as variáveis.

## 2. Configuração e variáveis de ambiente

Na primeira execução:

```sh
cp .env.example .env
```

Se `.env` já existir, confira seus valores em vez de sobrescrevê-lo. [.env.example](.env.example) contém somente valores locais de exemplo, sem segredos reais. `.env` e `.local/` são ignorados pelo Git.

O Compose lê `.env` automaticamente para interpolar sua configuração. O executável Go não carrega esse arquivo sozinho. O arquivo [compose.yaml](compose.yaml) também define valores internos dos containers: por exemplo, `postgres`, `keycloak` e `ministack` substituem `localhost` nos endereços de comunicação interna.

| Variável | Exemplo/padrão local | Uso |
| --- | --- | --- |
| `HTTP_PORT` | `8080` | Porta publicada da API; no container ela escuta em `8080` |
| `POSTGRES_HOST_PORT` | `5432` | Porta do PostgreSQL na máquina |
| `POSTGRES_DB` | `backend_challenge` | Banco de desenvolvimento |
| `POSTGRES_USER` | `challenge` | Usuário local do banco |
| `POSTGRES_PASSWORD` | `local_development_only` | Senha local de exemplo |
| `DATABASE_URL` | Definida em `.env.example` | Conexão usada ao executar Go na máquina; o Compose monta a conexão interna |
| `POSTGRES_TEST_URL` | Definida em `.env.example` | Conexão dos testes PostgreSQL; exige permissão para criar bancos descartáveis |
| `KEYCLOAK_HOST_PORT` | `8081` | Porta do Keycloak na máquina |
| `OIDC_ISSUER_URL` | `http://localhost:8081/realms/wallet` | Emissor esperado dos tokens |
| `OIDC_JWKS_URL` | `http://localhost:8081/realms/wallet/protocol/openid-connect/certs` | Endpoint de chaves; dentro do Compose usa o host `keycloak` |
| `OIDC_AUDIENCE` | `wallet-api` | Audiência esperada dos tokens |
| `SQS_ENABLED` | `true` | Habilita SQS na execução direta; no Compose está habilitado explicitamente |
| `AWS_REGION` | `us-east-1` | Região do cliente SQS; o Compose define esse valor explicitamente |
| `SQS_HOST_PORT` | `4566` | Porta do MiniStack na máquina |
| `SQS_ENDPOINT_URL` | `http://localhost:4566` | Endpoint local na execução direta; dentro do Compose usa `ministack` |
| `AWS_SHARED_CREDENTIALS_FILE` | `.local/sqs-runtime-credentials` | Credenciais restritas para executar a API na máquina; no container usa `/run/aws/credentials` |
| `AWS_EC2_METADATA_DISABLED` | `true` | Evita busca de credenciais de instância no ambiente local |
| `WORKERS_ENABLED` | `true` | Habilita publicação da outbox e retomada de pendências |
| `OUTBOX_MAX_ATTEMPTS` | `8` | Tentativas normais de entrega antes do encaminhamento à DLQ |
| `REFERENCE_MAX_ATTEMPTS` | `100` | Limite de tentativas de resolução de referências |
| `REFERENCE_TTL` | `24h` | Prazo de espera contado desde a criação da operação |

Se alterar portas, ajuste também as URLs usadas na execução direta e nos exemplos abaixo. Alterar usuário/senha no `.env` não recria usuários de um volume PostgreSQL já inicializado. Erros como `role "challenge" does not exist` podem indicar um volume criado com outras credenciais; alinhe a configuração ao banco existente ou use um ambiente separado.

## 3. Inicialização completa com Docker Compose

```sh
docker compose up --build -d
docker compose ps -a
```

O ambiente inicia PostgreSQL `17.11`, aplica as três migrations com `migrate v4.20.1`, importa o realm do Keycloak `26.8.0` e provisiona o MiniStack `1.5.30`. A API inicia após os serviços de preparação concluírem com sucesso.

`migrate` e `sqs-setup` são tarefas de preparação: é esperado que terminem com código `0`. API, PostgreSQL, Keycloak e MiniStack permanecem em execução.

Confira a disponibilidade:

```sh
curl --fail-with-body http://localhost:8080/health/live
curl --fail-with-body http://localhost:8080/health/ready
```

A readiness verifica PostgreSQL, schema, IdP, consumidor SQS e workers habilitados. Aguarde a inicialização do ambiente antes de fazer chamadas de negócio.

Para acompanhar logs, consultar métricas e parar o ambiente:

```sh
docker compose logs -f api
curl --fail-with-body http://localhost:8080/metrics
docker compose down
```

`docker compose down` preserva os volumes nomeados e os dados do PostgreSQL.

## 4. Migrations: aplicação e reversão

O fluxo completo do Compose já aplica as migrations. Para prepará-las separadamente:

```sh
docker compose up -d postgres
docker compose run --rm migrate up
docker compose run --rm migrate version
```

Após aplicar todas, a versão esperada é `3`:

| Versão | Responsabilidade |
| --- | --- |
| `000001_initial` | Carteiras, transações, ledger, outbox e invariantes financeiras |
| `000002_inbox` | Deduplicação persistente do consumidor |
| `000003_workers` | Leases da outbox e agenda de referências pendentes |

Para reverter a migration mais recente, pare a API antes:

```sh
docker compose stop api
docker compose run --rm migrate down 1
```

A reversão remove estruturas e dados pertencentes àquela versão. Repita `down 1` somente se também quiser reverter versões anteriores. Para reaplicar e reiniciar:

```sh
docker compose run --rm migrate up
docker compose up -d api
```

## 5. Filas e permissões da mensageria

O serviço `sqs-setup` cria automaticamente as filas e configura FIFO, redrive e permissões:

| Fila | Papel |
| --- | --- |
| `wager-transactions.fifo` | Solicitações de operações |
| `wager-transactions-dlq.fifo` | Solicitações que esgotaram o tratamento |
| `wallet-events.fifo` | Eventos publicados pela outbox |
| `wallet-events-dlq.fifo` | Eventos com falha persistente de entrega |

A visibilidade das filas principais é de `60` segundos. O redrive das solicitações permite cinco recebimentos. O consumidor usa backoff em falhas e apaga a mensagem somente após o commit do tratamento durável. Uma pendência de referência persistida permite concluir a inbox; sua continuidade pertence ao worker.

A outbox usa leases de `60` segundos e backoff persistente. Após o orçamento de entrega ou um erro permanente de publicação, encaminha o mesmo envelope para a DLQ de eventos. Uma falha na DLQ também mantém retry. Republicações preservam `eventId`; consumidores de eventos devem deduplicar por esse identificador.

As solicitações são internas: somente um publicador confiável tem permissão de envio. Provedores externos usam HTTP/Keycloak e não recebem credenciais da fila. O produtor interno deve validar a identidade de origem e o `providerId` antes de publicar.

No Compose, o setup cria usuários IAM locais separados para runtime da API, publicador de solicitações e consumidor de eventos, com credenciais em volumes distintos. A API recebe apenas seu volume em modo de leitura. `test/test` é uma credencial administrativa exclusiva do setup local.

Para repetir o provisionamento, com o broker em execução:

```sh
docker compose run --rm sqs-setup
```

O emulador executa com `AUTH=true`, mas mantém acesso administrativo root/sem assinatura. Use-o somente no ambiente local. As políticas das identidades restritas são exercitadas pelos testes; isso não equivale à validação de segurança em AWS.

Em AWS, use roles administradas pelo deployment, omita `SQS_ENDPOINT_URL` e `SQS_LOCAL_IAM_SETUP` e execute `/sqs-setup` com credenciais de provisionamento e quatro ARNs válidos e distintos:

- `SQS_ADMIN_PRINCIPAL_ARN`
- `SQS_RUNTIME_PRINCIPAL_ARN`
- `SQS_TRUSTED_PUBLISHER_PRINCIPAL_ARN`
- `SQS_EVENT_CONSUMER_PRINCIPAL_ARN`

O setup aplica políticas de recurso com os ARNs reais das filas. As roles, políticas de identidade e relações de confiança pertencem ao deployment. Os modelos estão em [deploy/iam](deploy/iam).

## 6. Autenticação local

O Keycloak importa automaticamente [deploy/keycloak/wallet-realm.json](deploy/keycloak/wallet-realm.json). Não é necessário cadastrar usuários ou clientes manualmente.

| Cliente | Segredo local de exemplo | Permissão |
| --- | --- | --- |
| `wallet-service` | `wallet-service-local-secret` | Operações internas de carteira |
| `provider-a` | `provider-a-local-secret` | Transações do `provider-a` |
| `provider-b` | `provider-b-local-secret` | Transações do `provider-b` |

Esses valores são fixtures de desenvolvimento. O painel administrativo local do Keycloak fica em `http://localhost:8081`, com usuário `admin` e senha `admin_local_only`.

Prepare as URLs dos exemplos e obtenha tokens com `client_credentials`:

```sh
API_URL=http://localhost:8080
KEYCLOAK_URL=http://localhost:8081

INTERNAL_TOKEN=$(curl --fail --silent --show-error \
  "$KEYCLOAK_URL/realms/wallet/protocol/openid-connect/token" \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode client_id=wallet-service \
  --data-urlencode client_secret=wallet-service-local-secret \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')

PROVIDER_TOKEN=$(curl --fail --silent --show-error \
  "$KEYCLOAK_URL/realms/wallet/protocol/openid-connect/token" \
  --data-urlencode grant_type=client_credentials \
  --data-urlencode client_id=provider-a \
  --data-urlencode client_secret=provider-a-local-secret \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')
```

Os tokens expiram. Se uma chamada passar a retornar `401`, obtenha novamente o token correspondente. Para o provedor B, repita a obtenção com `client_id=provider-b` e seu segredo local.

## 7. Exemplos HTTP

### Abrir uma carteira

```sh
RUN_ID=$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')
PLAYER_ID="player-manual-$RUN_ID"

WALLET_RESPONSE=$(curl --fail --silent --show-error \
  "$API_URL/wallets" \
  -H "Authorization: Bearer $INTERNAL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER_ID\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}")

printf '%s\n' "$WALLET_RESPONSE"
WALLET_ID=$(printf '%s' "$WALLET_RESPONSE" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
```

Esperado: carteira com saldo `100.00 BRL` e versão `1`. Essa abertura também persiste `OPENING`, ledger e eventos no mesmo commit. Abrir novamente a mesma combinação jogador/moeda retorna `409`.

### Enviar uma aposta e repetir a mesma operação

```sh
BET_ID="bet-$RUN_ID"
BET_KEY="provider-a:$BET_ID"
BET_BODY="{\"providerId\":\"provider-a\",\"externalTransactionId\":\"$BET_ID\",\"playerId\":\"$PLAYER_ID\",\"walletId\":\"$WALLET_ID\",\"roundId\":\"round-$RUN_ID\",\"gameId\":\"manual-game\",\"kind\":\"BET\",\"money\":{\"amount\":\"25.00\",\"currency\":\"BRL\"}}"

curl --fail-with-body "$API_URL/wagering/transactions" \
  -H "Authorization: Bearer $PROVIDER_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $BET_KEY" \
  -H "X-Correlation-Id: manual-$RUN_ID" \
  -d "$BET_BODY"
```

Esperado: `200`, `status=PROCESSED`, saldo `75.00 BRL` e `idempotentReplay=false`. Repita o mesmo comando: o resultado mantém a identidade da transação e o saldo original, com `idempotentReplay=true`, sem novo débito. Alterar o conteúdo usando a mesma chave retorna `409`.

### Consultar a operação, carteira e ledger; reconciliar

```sh
curl --fail-with-body "$API_URL/providers/provider-a/wagering/transactions/$BET_ID" \
  -H "Authorization: Bearer $PROVIDER_TOKEN"

curl --fail-with-body "$API_URL/wallets/$WALLET_ID" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"

curl --fail-with-body "$API_URL/wallets/$WALLET_ID/ledger?limit=50" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"

curl --fail-with-body -X POST "$API_URL/wallets/$WALLET_ID/reconciliation" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"
```

A reconciliação deve retornar saldo armazenado e calculado de `75.00 BRL`, diferença `0.00`, `consistent=true` e dois lançamentos. A resposta do ledger pode incluir `nextCursor`; envie esse valor codificado como parâmetro `cursor` para continuar a paginação. Também existe `GET /wagering/transactions/{transactionId}`, usando o identificador interno devolvido pela aposta.

Para `WIN`, `LOSS`, `REFUND` ou `ROLLBACK`, use uma nova identidade externa e nova chave. `LOSS` exige `0.00`. Reversões exigem `referenceExternalTransactionId`, mesma rodada e valor integral da referência. Referências ainda ausentes geram `202/PENDING_REFERENCE` e são retomadas pelos workers.

### Respostas e permissões

| HTTP | Situação |
| --- | --- |
| `201` | Carteira criada |
| `200` | Consulta, operação processada ou replay processado |
| `202` | Referência pendente |
| `400` | Entrada inválida, incluindo dinheiro como número JSON |
| `401` | Token ausente, inválido ou expirado |
| `403` | Identidade sem permissão ou provedor incompatível |
| `404` | Recurso não encontrado |
| `409` | Conflito de unicidade ou idempotência |
| `422` | Resultado terminal `REJECTED` ou `FAILED`, com `failureCode` |
| `503` | Falha que impediu confirmar o processamento; repetir com a mesma chave e conteúdo |

Erros de entrada/acesso usam `{"error":{"code":"...","message":"..."}}`. Resultados financeiros terminais usam `transactionId`, `status`, `failureCode` quando aplicável e `idempotentReplay`. `FAILED` é reservado no domínio: não há encerramento automático nesse estado nos adaptadores atuais. Rejeições como `INSUFFICIENT_FUNDS` são persistidas e reproduzidas em replays. A política detalhada está em [ARCHITECTURE.md](ARCHITECTURE.md#política-de-failed).

## 8. Exemplo SQS com deduplicação entre HTTP e mensageria

Este exemplo reutiliza a aposta da seção anterior. Ele deve concluir a inbox sem aplicar outro débito.

Copie as credenciais do publicador confiável:

```sh
mkdir -p .local
docker compose cp sqs-setup:/credentials/publisher/credentials .local/sqs-publisher-credentials
chmod 600 .local/sqs-publisher-credentials
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_PROFILE
export AWS_SHARED_CREDENTIALS_FILE="$PWD/.local/sqs-publisher-credentials"
export AWS_DEFAULT_REGION=us-east-1
export SQS_ENDPOINT_URL=http://localhost:4566
```

Monte o envelope e os identificadores FIFO:

```sh
MESSAGE_ID="manual-sqs-$RUN_ID"
python3 - "$BET_BODY" "$BET_KEY" "$MESSAGE_ID" > .local/wager-request.json <<'PYTHON'
import datetime, json, sys
body = json.loads(sys.argv[1])
body["idempotencyKey"] = sys.argv[2]
json.dump({
    "messageId": sys.argv[3],
    "type": "WagerTransactionRequested",
    "occurredAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "data": body,
}, sys.stdout)
PYTHON

QUEUE_URL=$(aws --endpoint-url "$SQS_ENDPOINT_URL" sqs get-queue-url \
  --queue-name wager-transactions.fifo --query QueueUrl --output text)

MESSAGE_GROUP_ID=$(python3 -c 'import hashlib,json; d=json.load(open(".local/wager-request.json"))["data"]; print(hashlib.sha256(json.dumps([d["providerId"],d["walletId"]],separators=(",",":"),ensure_ascii=False).encode()).hexdigest())')
MESSAGE_DEDUP_ID=$(python3 -c 'import hashlib,json; m=json.load(open(".local/wager-request.json"))["messageId"]; print(hashlib.sha256(m.encode()).hexdigest())')

aws --endpoint-url "$SQS_ENDPOINT_URL" sqs send-message \
  --queue-url "$QUEUE_URL" \
  --message-body file://.local/wager-request.json \
  --message-group-id "$MESSAGE_GROUP_ID" \
  --message-deduplication-id "$MESSAGE_DEDUP_ID"
```

`MessageGroupId` é SHA-256 do JSON compacto UTF-8 `[providerId,walletId]`. `MessageDeduplicationId` é SHA-256 do `messageId` do envelope. Esses valores roteiam/deduplicam no broker; a garantia financeira continua no PostgreSQL. A janela FIFO pode impedir reenvios de chegarem ao consumidor: para exercitar a inbox, reenvie o mesmo envelope com um novo identificador de deduplicação do broker.

A confirmação de envio da AWS CLI comprova apenas o aceite no broker. Acompanhe o processamento nos logs e confira a inbox:

```sh
docker compose exec -T postgres psql -U challenge -d backend_challenge \
  -c "SELECT consumer_name, message_id, completed_at, transaction_id FROM inbox_messages WHERE message_id = '$MESSAGE_ID';"
```

Após o processamento, deve existir uma entrada concluída ligada à mesma aposta HTTP; o saldo permanece `75.00 BRL`. A consulta usa os nomes padrão de usuário/banco: ajuste-os se modificou `.env`.

Os quatro eventos de saída são `WagerTransactionProcessed`, `WagerTransactionRejected`, `WalletBalanceChanged` e `WagerTransactionPendingReference`. São envelopes JSON versionados, com correlação, causa opcional e dados tipados; dinheiro é string decimal. `MessageGroupId` de saída é SHA-256 de `wallet:<walletId>` e `MessageDeduplicationId` é SHA-256 de `eventId`. O consumidor externo usa sua própria identidade; suas credenciais locais ficam em `/credentials/events/credentials` no container `sqs-setup`.

## 9. Execução da API diretamente com Go

Primeiro prepare dependências, migrations, IdP e filas:

```sh
docker compose build sqs-setup
docker compose up -d postgres keycloak ministack sqs-setup
docker compose run --rm migrate up
docker compose stop api
mkdir -p .local
```

Espere o `sqs-setup` concluir com código `0` e o Keycloak ficar saudável, consultando `docker compose ps -a`. Copie as credenciais de runtime geradas pelo setup:

```sh
docker compose cp sqs-setup:/credentials/runtime/credentials .local/sqs-runtime-credentials
chmod 600 .local/sqs-runtime-credentials
```

Exporte a configuração local e execute:

```sh
set -a
. ./.env
set +a
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN AWS_PROFILE
go run ./cmd/api
```

Confira se as URLs em `.env` apontam para as portas locais corretas. Use `Ctrl+C` para encerrar. O ciclo de vida Fx interrompe novas entradas e aguarda os trabalhos em andamento dentro dos prazos configurados antes de fechar os recursos.

## 10. Testes e verificações

### Unitários e análise estática

```sh
go test ./...
go test -race ./...
go vet ./...
```

Esses comandos não incluem os arquivos com build tag `integration`.

### Suíte completa com infraestrutura real

```sh
./scripts/test-system.sh
```

O script exige Docker/Compose e Go com suporte a `-race`. Compila o executável real, prepara containers isolados de PostgreSQL, Keycloak e MiniStack, executa os testes com `-race -count=1 -tags=integration`, roda `go vet` com a mesma tag e remove os recursos temporários ao terminar. Os testes criam bancos descartáveis; não usam o banco de desenvolvimento. A execução pode levar alguns minutos, inclusive pela espera da visibilidade real do SQS.

### Integração HTTP e token expirado

```sh
./scripts/test-system.sh -run '^TestHTTP' -v
```

Inclui PostgreSQL/Keycloak reais, abertura, replay, rejeição, isolamento entre provedores, permissões internas e token realmente expirado. `scripts/test-http-integration.sh` é um alias para a suíte completa.

### Permissões do broker

```sh
./scripts/test-system.sh -run '^TestSQSBrokerAuthorization$' -v
```

Verifica identidades restritas, envio e consumo autorizados, negações administrativas, mensagens em lote, credenciais desconhecidas e assinaturas inválidas.

### Três instâncias, concorrência e simulações de falha

```sh
./scripts/test-system.sh -run '^TestDistributedInstancesAndFailures$' -v
```

O próprio teste inicia três processos independentes da API, cada um com conexões e memória próprias. Exercita 50 apostas HTTP concorrentes com reentregas SQS da mesma operação, duas apostas de `80.00` sobre saldo `100.00`, carteiras independentes, publishers concorrentes, reinícios, referências pendentes, falhas entre commit/envio/confirmação, retries, DLQ, indisponibilidade das dependências, readiness, composição Fx e reconciliação final do ledger.

### Somente PostgreSQL contra uma dependência já iniciada

```sh
docker compose up -d postgres
export POSTGRES_TEST_URL='postgres://challenge:local_development_only@localhost:5432/backend_challenge?sslmode=disable'
go test -race -count=1 -tags=integration ./internal/postgres -timeout=120s
go vet -tags=integration ./internal/postgres
```

Use credenciais compatíveis com seu `.env`, com permissão de criação de bancos. Os testes verificam migrations/reversões, constraints, imutabilidade, atomicidade, replay persistente, inbox e leases.

## 11. Observabilidade e limites

`/metrics` expõe resultados por estado/replay, retries, DLQ, conflitos, latências, pendências e atraso da outbox e divergências de reconciliação. Os logs JSON incluem os identificadores disponíveis; `transactionId` permite ligar o recebimento à retomada e à publicação mesmo quando a correlação da retomada difere da original.

PostgreSQL/SQS indisponíveis permitem retry. Falhas de publicação são tratadas na outbox e não alteram uma operação financeira já processada. Não há uma causa irreversível habilitada para encerrar automaticamente em `FAILED`. As decisões sobre dinheiro, hashes, locks, referências, reversões, inbox/outbox, permissões e shutdown estão em [ARCHITECTURE.md](ARCHITECTURE.md).

Tracing, dashboards, partidas dobradas e testes de carga não foram implementados e são opcionais no desafio. Os comandos de teste acima permitem verificar a versão local; este guia não substitui o resultado de uma execução da suíte.

## 12. Roteiro de testes em ordem

Este roteiro reúne os comandos e a finalidade de cada verificação. Execute na raiz do projeto, na mesma sessão de terminal, com as portas padrão. Requer Docker/Compose, Go, `curl` e Python 3. Os testes manuais criam dados no banco local de desenvolvimento; a suíte automatizada usa infraestrutura temporária isolada. Nenhum comando abaixo envia alterações ao GitHub.

Para repetir o roteiro, comece pela etapa 3 para gerar novos identificadores e obter tokens atualizados. Siga a ordem: os saldos esperados dependem das operações anteriores. As funções `get_token`, `make_body` e `send_wager` são auxiliares do terminal, sem alteração no código da aplicação.

### Etapa 1 — Preparar ambiente e migrations

**Finalidade:** verificar build, inicialização das dependências, provisionamento de filas/identidades e versão do schema.

```sh
[ -f .env ] || cp .env.example .env
docker compose up --build -d
docker compose ps -a
docker compose run --rm migrate version
```

**Esperado:** API, PostgreSQL, Keycloak e MiniStack em execução; `migrate` e `sqs-setup` concluídos com `Exited (0)`. A versão das migrations deve ser `3`, sem indicação de estado sujo. Confira se os valores de um `.env` já existente correspondem ao ambiente local descrito neste README.

### Etapa 2 — Verificar saúde

**Finalidade:** distinguir processo disponível de dependências prontas para uso.

```sh
curl -i http://localhost:8080/health/live
curl -i http://localhost:8080/health/ready
```

**Esperado:** HTTP `200` em ambos. Na readiness, PostgreSQL, schema, IdP, SQS e workers habilitados devem estar `ready`.

### Etapa 3 — Obter tokens e criar identificadores

**Finalidade:** usar identidades reais do Keycloak e separar os dados de cada execução manual.

```sh
API_URL=http://localhost:8080
KEYCLOAK_URL=http://localhost:8081

get_token() {
  curl --fail --silent --show-error \
    "$KEYCLOAK_URL/realms/wallet/protocol/openid-connect/token" \
    --data-urlencode grant_type=client_credentials \
    --data-urlencode "client_id=$1" \
    --data-urlencode "client_secret=$1-local-secret" \
    | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])'
}

INTERNAL_TOKEN=$(get_token wallet-service)
PROVIDER_TOKEN=$(get_token provider-a)
PROVIDER_B_TOKEN=$(get_token provider-b)

RUN_ID=$(python3 -c 'import uuid; print(uuid.uuid4().hex[:12])')
PLAYER_ID="player-test-$RUN_ID"
```

**Esperado:** nenhum erro. As atribuições normalmente não exibem saída. Não é necessário imprimir os tokens.

Os tokens expiram. Se receber `401` inesperado durante o roteiro, renove a identidade usada e repita a chamada que falhou:

```sh
INTERNAL_TOKEN=$(get_token wallet-service)
PROVIDER_TOKEN=$(get_token provider-a)
PROVIDER_B_TOKEN=$(get_token provider-b)
```

Renovar tokens não exige recriar a carteira nem alterar os identificadores das operações.

### Etapa 4 — Abrir uma carteira e verificar duplicidade

**Finalidade:** validar abertura positiva e unicidade por jogador/moeda.

```sh
WALLET_RESPONSE=$(curl --fail --silent --show-error \
  "$API_URL/wallets" \
  -H "Authorization: Bearer $INTERNAL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER_ID\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}")

printf '%s\n' "$WALLET_RESPONSE"
WALLET_ID=$(printf '%s' "$WALLET_RESPONSE" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
```

**Esperado:** saldo `100.00 BRL`, versão `1` e identidade salva em `WALLET_ID`.

Tente abrir novamente para o mesmo jogador e moeda, sem sobrescrever as variáveis da carteira criada:

```sh
curl -i "$API_URL/wallets" \
  -H "Authorization: Bearer $INTERNAL_TOKEN" \
  -H 'Content-Type: application/json' \
  -d "{\"playerId\":\"$PLAYER_ID\",\"initialBalance\":{\"amount\":\"100.00\",\"currency\":\"BRL\"}}"
```

**Esperado:** `409 Conflict`.

### Etapa 5 — Preparar funções auxiliares de operações

**Finalidade:** montar o mesmo contrato JSON e os headers necessários sem repetir todos os campos a cada chamada.

`make_body` recebe tipo, identidade externa, valor decimal e referência opcional. `send_wager` recebe o sufixo da chave e o corpo; sua saída inclui o código HTTP.

```sh
make_body() {
  python3 - "$1" "$2" "$3" "${4:-}" "$PLAYER_ID" "$WALLET_ID" "$RUN_ID" <<'PY'
import json, sys

kind, external_id, amount, reference, player, wallet, run = sys.argv[1:]
body = {
    "providerId": "provider-a",
    "externalTransactionId": external_id,
    "playerId": player,
    "walletId": wallet,
    "roundId": "round-" + run,
    "gameId": "manual-game",
    "kind": kind,
    "money": {"amount": amount, "currency": "BRL"},
}
if reference:
    body["referenceExternalTransactionId"] = reference
print(json.dumps(body))
PY
}

send_wager() {
  curl --silent --show-error \
    -w '\nHTTP %{http_code}\n' \
    "$API_URL/wagering/transactions" \
    -H "Authorization: Bearer $PROVIDER_TOKEN" \
    -H 'Content-Type: application/json' \
    -H "Idempotency-Key: provider-a:$1" \
    -H "X-Correlation-Id: manual-$RUN_ID" \
    -d "$2"
}
```

**Esperado:** as funções são definidas sem saída. O envio seguinte comprova seu funcionamento. O helper exibe respostas de erro para análise, sem tratar HTTP `4xx` esperado como falha de execução do shell.

### Etapa 6 — Testar BET, replay e conflitos

**Finalidade:** comprovar débito único, identidade persistente e rejeição de reutilização indevida de chave ou ID externo.

```sh
BET_ID="bet-$RUN_ID"
BET_BODY=$(make_body BET "$BET_ID" 25.00)
send_wager "$BET_ID" "$BET_BODY"
```

**Esperado:** `200/PROCESSED`, saldo `75.00` e `idempotentReplay=false`.

```sh
send_wager "$BET_ID" "$BET_BODY"
```

**Esperado:** mesma transação, saldo original `75.00` e `idempotentReplay=true`.

```sh
CONFLICT_BODY=$(make_body BET "$BET_ID" 26.00)
send_wager "$BET_ID" "$CONFLICT_BODY"
send_wager "another-key-$RUN_ID" "$BET_BODY"
```

**Esperado:** `409` nas duas chamadas. A primeira reutiliza a chave com conteúdo diferente; a segunda tenta usar outra chave para a mesma operação externa. Nenhuma deve aplicar outro débito.

### Etapa 7 — Testar WIN, LOSS, REFUND e ROLLBACK

**Finalidade:** conferir os demais tipos externos, seus efeitos e a proteção contra reversão duplicada.

Execute na ordem:

```sh
WIN_ID="win-$RUN_ID"
send_wager "$WIN_ID" "$(make_body WIN "$WIN_ID" 10.00)"

LOSS_ID="loss-$RUN_ID"
send_wager "$LOSS_ID" "$(make_body LOSS "$LOSS_ID" 0.00)"

REFUND_ID="refund-$RUN_ID"
send_wager "$REFUND_ID" "$(make_body REFUND "$REFUND_ID" 25.00 "$BET_ID")"

ROLLBACK_ID="rollback-$RUN_ID"
send_wager "$ROLLBACK_ID" "$(make_body ROLLBACK "$ROLLBACK_ID" 10.00 "$WIN_ID")"
```

| Operação | HTTP/estado esperado | Saldo original do resultado |
| --- | --- | --- |
| WIN de `10.00` | `200/PROCESSED` | `85.00` |
| LOSS de `0.00` | `200/PROCESSED` | `85.00` |
| REFUND integral da BET | `200/PROCESSED` | `110.00` |
| ROLLBACK integral do WIN | `200/PROCESSED` | `100.00` |

`LOSS` não incrementa a versão e não cria ledger. Tente uma segunda devolução da mesma aposta:

```sh
DUP_REFUND_ID="duplicate-refund-$RUN_ID"
send_wager "$DUP_REFUND_ID" \
  "$(make_body REFUND "$DUP_REFUND_ID" 25.00 "$BET_ID")"
```

**Esperado:** `422/REJECTED`, com `failureCode=ALREADY_REVERSED`.

### Etapa 8 — Testar saldo insuficiente e replay de rejeição

**Finalidade:** verificar recusa auditável sem alterar o saldo e reprodução do resultado rejeitado.

```sh
LOW_BET_ID="insufficient-$RUN_ID"
send_wager "$LOW_BET_ID" "$(make_body BET "$LOW_BET_ID" 101.00)"
send_wager "$LOW_BET_ID" "$(make_body BET "$LOW_BET_ID" 101.00)"
```

**Esperado:** ambas retornam `422/REJECTED`, com `INSUFFICIENT_FUNDS` e a mesma identidade interna. A segunda tem `idempotentReplay=true`. O saldo permanece `100.00`.

### Etapa 9 — Rejeitar dinheiro inválido e OPENING externo

**Finalidade:** conferir validação de transporte/formato antes de aceitar uma operação financeira.

Dinheiro como número JSON:

```sh
INVALID_ID="invalid-money-$RUN_ID"
INVALID_BODY=$(make_body BET "$INVALID_ID" 1.25 \
  | python3 -c 'import json,sys; b=json.load(sys.stdin); b["money"]["amount"]=1.25; print(json.dumps(b))')
send_wager "$INVALID_ID" "$INVALID_BODY"
```

Negativos, escala excedente, notação científica e abertura externa:

```sh
send_wager "negative-$RUN_ID" "$(make_body BET "negative-$RUN_ID" -1.00)"
send_wager "scale-$RUN_ID" "$(make_body BET "scale-$RUN_ID" 1.001)"
send_wager "scientific-$RUN_ID" "$(make_body BET "scientific-$RUN_ID" 1e2)"
send_wager "opening-$RUN_ID" "$(make_body OPENING "opening-$RUN_ID" 10.00)"
```

**Esperado:** HTTP `400` em todas, sem movimentações financeiras.

### Etapa 10 — Testar autenticação e isolamento

**Finalidade:** conferir recusa de credenciais inválidas e restrição das identidades de provedor.

```sh
curl -i "$API_URL/wallets/$WALLET_ID"

curl -i "$API_URL/wallets/$WALLET_ID" \
  -H 'Authorization: Bearer invalid-token'

curl -i "$API_URL/wallets/$WALLET_ID" \
  -H "Authorization: Bearer $PROVIDER_TOKEN"

curl -i "$API_URL/providers/provider-a/wagering/transactions/$BET_ID" \
  -H "Authorization: Bearer $PROVIDER_B_TOKEN"
```

**Esperado, na ordem:** `401`, `401`, `403`, `403`. Para interpretar os `403` corretamente, os tokens dos provedores precisam continuar válidos; renove-os se necessário. A expiração real é coberta pela suíte da etapa 15 e pelo filtro da etapa 16.

### Etapa 11 — Conferir saldo, ledger, reconciliação e saldo original

**Finalidade:** verificar que os movimentos persistidos correspondem ao saldo atual e que o replay preserva o resultado original.

```sh
curl --fail-with-body "$API_URL/wallets/$WALLET_ID" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"

curl --fail-with-body "$API_URL/wallets/$WALLET_ID/ledger?limit=50" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"

curl --fail-with-body -X POST "$API_URL/wallets/$WALLET_ID/reconciliation" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"

send_wager "$BET_ID" "$BET_BODY"
```

**Esperado:** carteira com saldo `100.00`, versão `5`, cinco lançamentos e reconciliação com `consistent=true`, diferença `0.00` e `checkedEntries=5`. Os lançamentos são abertura, BET, WIN, REFUND e ROLLBACK. O replay da BET continua retornando o saldo original `75.00`, com `idempotentReplay=true`.

### Etapa 12 — Entregar uma reversão antes da referência

**Finalidade:** comprovar espera persistente e retomada automática pelo worker.

```sh
EARLY_REFUND_ID="early-refund-$RUN_ID"
LATE_BET_ID="late-bet-$RUN_ID"

send_wager "$EARLY_REFUND_ID" \
  "$(make_body REFUND "$EARLY_REFUND_ID" 5.00 "$LATE_BET_ID")"
```

**Esperado:** `202/PENDING_REFERENCE`, sem alteração de saldo.

```sh
send_wager "$LATE_BET_ID" "$(make_body BET "$LATE_BET_ID" 5.00)"
```

**Esperado:** `200/PROCESSED`, com saldo original `95.00`.

```sh
curl --fail-with-body \
  "$API_URL/providers/provider-a/wagering/transactions/$EARLY_REFUND_ID" \
  -H "Authorization: Bearer $PROVIDER_TOKEN"
```

Repita a consulta até o worker concluir. O backoff pode exigir aguardar alguns segundos entre tentativas.

**Esperado:** reembolso `PROCESSED`, saldo original `100.00` e `referenceTransactionId` vinculado à aposta tardia.

```sh
INTERNAL_TOKEN=$(get_token wallet-service)
curl --fail-with-body -X POST "$API_URL/wallets/$WALLET_ID/reconciliation" \
  -H "Authorization: Bearer $INTERNAL_TOKEN"
```

**Esperado após concluir o reembolso:** saldo armazenado e calculado `100.00`, diferença `0.00`, `consistent=true` e `checkedEntries=7`.

### Etapa 13 — Conferir outbox, métricas e logs

**Finalidade:** acompanhar publicação e retomada usando o estado durável, as métricas e os identificadores dos logs.

Com os nomes padrão de usuário/banco:

```sh
docker compose exec -T postgres psql -U challenge -d backend_challenge \
  -c "SELECT event_type, published_at IS NOT NULL AS published, dead_lettered_at IS NOT NULL AS dead_lettered FROM outbox_events WHERE aggregate_id = '$WALLET_ID' ORDER BY occurred_at, event_id;"
```

**Esperado:** sete eventos `WalletBalanceChanged`, publicados (`t`) e sem DLQ (`f`) após o worker concluir. Essa consulta seleciona os eventos agregados pela carteira; eventos de transação têm o ID da operação como agregado e não aparecem nesse filtro. Ajuste usuário/banco se alterou `.env`.

```sh
curl --fail-with-body "$API_URL/metrics" \
  | grep -E '^wallet_(wager_results_total|background_operations_total|outbox_|reference_jobs_pending|reconciliations_total|storage_metrics_up)'

docker compose logs --no-color api \
  | grep -E "manual-$RUN_ID|$WALLET_ID"
```

**Esperado:** métricas de resultados, publicação, espera e reconciliação; em repouso, `wallet_outbox_pending=0`, `wallet_reference_jobs_pending=0` e `wallet_storage_metrics_up=1`. Outros dados já existentes no ambiente podem influenciar os totais globais.

Nos logs, observe operações HTTP, eventos publicados e o reembolso retomado. Use `transactionId` para ligar o recebimento original à retomada, cuja correlação é o ID da operação. Os comandos usam `grep`, sem exigir instalação de `rg`.

### Etapa 14 — Testar domínio, casos de uso e análise estática

**Finalidade:** cobrir parsing, escala, overflow, moedas, invariantes, transições, reidratação, reversões, hashes e rollback, além de procurar condições de corrida e problemas estáticos.

```sh
go test -count=1 -v ./internal/domain ./internal/application
go test -race -count=1 ./...
go vet ./...
```

**Esperado:** aprovação, sem `FAIL` ou `WARNING: DATA RACE`; `go vet` normalmente termina sem saída quando não encontra problemas. Os comandos desta etapa não incluem os testes com tag `integration`.

### Etapa 15 — Executar toda a integração e validação distribuída

**Finalidade:** comprovar as garantias entre processos e nas fronteiras de falha usando PostgreSQL, Keycloak e MiniStack reais.

```sh
./scripts/test-system.sh -v
```

O script compila a API com `-race`, prepara infraestrutura isolada, executa a suíte completa com tag `integration`, roda `go vet` com essa tag e remove os recursos temporários. Pode levar alguns minutos; há esperas pela visibilidade real do SQS.

| Verificação | Resultado esperado |
| --- | --- |
| Mesma aposta 50 vezes e reentregas SQS | Um único débito; inbox/idempotência impedem duplicidade |
| Duas apostas de `80.00` sobre saldo `100.00` | Uma processada, outra rejeitada; saldo `20.00` |
| Carteiras distintas e três processos | Coordenação por carteira e avanço concorrente |
| Reinício de processos | Idempotência e referências pendentes preservadas |
| Queda após commit e antes de apagar a mensagem | Reentrega segura, sem repetir a movimentação |
| Publishers concorrentes e queda antes/depois do envio | Recuperação de reservas com identidade/payload do evento preservados |
| Limite e TTL de referência | Resolução ou rejeição durável com código apropriado |
| Mensagens inválidas e falhas de publicação | Retry e DLQ reais |
| Permissões do broker | Somente as identidades designadas enviam, consomem e administram |
| Keycloak real | Credenciais inválidas/expiradas rejeitadas e provedores isolados |
| Banco real | Constraints, ledger imutável, atomicidade, inbox, leases e migrations reversíveis |
| Dependências indisponíveis | Readiness sinaliza falha e operação recupera após retorno |
| Fx e falha parcial de startup | Inicialização/encerramento e liberação de recursos |
| Reconciliação final | Saldos iguais ao ledger e ausência de lançamentos duplicados |

**Esperado:** todos os testes aprovados, sem testes ignorados ou alertas de corrida, e limpeza dos containers temporários. Linhas `[no test files]` indicam pacotes sem arquivos próprios de teste naquela seleção; vários adaptadores são exercitados pelas suítes de integração e sistema.

### Etapa 16 — Repetir grupos para estudo ou diagnóstico

Os filtros abaixo são opcionais após a aprovação da suíte completa. Cada execução prepara novamente as dependências temporárias.

| Finalidade | Comando |
| --- | --- |
| API autenticada e permissões | `./scripts/test-system.sh -run '^TestHTTP' -v` |
| Token realmente expirado | `./scripts/test-system.sh -run '^TestHTTPRejectsExpiredKeycloakToken$' -v` |
| Autorizações da mensageria | `./scripts/test-system.sh -run '^TestSQSBrokerAuthorization$' -v` |
| PostgreSQL, migrations e constraints | `./scripts/test-system.sh -run '^TestPostgres' -v` |
| Três instâncias e falhas | `./scripts/test-system.sh -run '^TestDistributedInstancesAndFailures$' -v` |

Para estudar publicação manual de uma solicitação SQS, use o exemplo da seção **8. Exemplo SQS com deduplicação entre HTTP e mensageria** deste guia. A etapa 15 já inclui essa integração e comprova recebimentos repetidos na aplicação.

### Checklist de conclusão do roteiro

- [ ] Ambiente, IdP, filas e migrations preparados.
- [ ] Liveness/readiness retornando `200`.
- [ ] Carteira aberta e abertura duplicada rejeitada.
- [ ] BET, WIN, LOSS, REFUND e ROLLBACK com os resultados esperados.
- [ ] Replays e conflitos sem novos movimentos.
- [ ] Rejeição por saldo insuficiente persistida e reproduzida.
- [ ] Entradas monetárias inválidas e OPENING externo rejeitados.
- [ ] Autenticação e isolamento entre provedores conferidos.
- [ ] Referência pendente retomada e reconciliada.
- [ ] Saldo, ledger e versão compatíveis.
- [ ] Outbox publicada, métricas e logs conferidos.
- [ ] Testes unitários, detector de corridas e análise estática aprovados.
- [ ] Integração completa com infraestrutura real e três processos aprovada.

`FAILED` tem suas invariantes e transições testadas no domínio; não existe causa habilitada para produzi-lo automaticamente no runtime. Tracing, dashboards, partidas dobradas e testes de carga são diferenciais opcionais, fora deste roteiro obrigatório.
