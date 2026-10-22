# Arquitetura e decisões da solução

Este documento descreve a implementação atual e as interpretações adotadas para o desafio. O [README.md](README.md#guia-de-execução-da-solução) reúne configuração, execução, chamadas autenticadas e comandos de teste.

## Resumo das decisões exigidas pelo desafio

Cada seção detalha o comportamento implementado e suas consequências. A tabela reúne a escolha e a justificativa de cada tema solicitado no README.

| Tema | Decisão adotada | Justificativa e consequência | Detalhamento |
| --- | --- | --- | --- |
| Dinheiro | `int64` em unidades mínimas, escala fixa e moeda explícita | Precisão exata e persistência direta em `BIGINT`; exige verificar overflow e limitar moedas/escala | Seção 2 |
| Transações | Uma transação PostgreSQL por tratamento financeiro, com `pgx` e `READ COMMITTED` | Confirma saldo, operação, ledger, inbox aplicável e outbox juntos; erro de confirmação exige consulta/retry idempotente | Seção 4 |
| Idempotência | Chave e ID externo únicos por provedor, hash canônico e resultado persistido | Mantém a garantia entre processos e reinícios, inclusive entre HTTP e SQS; replays devolvem o resultado original | Seção 5 |
| Locks | Lock pessimista por carteira e reservas com `SKIP LOCKED` nos workers | Serializa disputas da mesma carteira e permite avanço de carteiras/trabalhos distintos; uma carteira muito disputada limita throughput | Seção 6 |
| Referências pendentes | Estado persistido, agenda durável, backoff, TTL e limite de tentativas | Aceita chegada fora de ordem e permite retomada por outra instância; espera esgotada produz rejeição de negócio | Seção 7 |
| Reversões | Reversão integral, referência compatível e uma reversão direta processada por referência | Evita devolução duplicada; a combinação de REFUND e ROLLBACK segue a interpretação restritiva documentada | Seção 8 |
| Inbox | Identidade consumidor/mensagem e hash na mesma transação do domínio | Deduplica reentregas de transporte sem depender da janela FIFO; reutilização com conteúdo diferente é conflito | Seção 9 |
| Outbox | Snapshot imutável no commit financeiro e publicação posterior por workers | Evita publicar antes do commit e recupera envio abandonado; entrega pode repetir e exige deduplicação por `eventId` | Seção 10 |
| Autenticação | Keycloak/OIDC com `client_credentials`, validação de assinatura, emissor, audiência e expiração | Delega emissão ao IdP e mantém identidade verificável; disponibilidade de chaves faz parte da operação | Seção 11 |
| Autorização | Ator interno/provedor no caso de uso e identidades/políticas IAM no broker | Restringe operações e isolamento financeiro; a fila compartilhada confia somente em produtores internos autorizados | Seção 11 |
| Uber Fx | Injeção por construtores, módulos e hooks na composição | Centraliza a montagem e a aquisição/liberação de recursos sem acoplar o domínio ao container | Seção 12 |
| Shutdown | Interromper novas entradas, drenar trabalho com prazos e fechar dependências ao final | Reduz trabalho interrompido; se o prazo acabar ou houver queda, a recuperação depende dos registros duráveis | Seção 12 |

Limitações, interpretações e trabalho não concluído estão explicitados na seção 15. A existência de uma entidade ou configuração no código não implica que toda extensão possível esteja conectada ao runtime; isso se aplica especialmente a `FAILED`.

## 1. Visão geral e organização

**Decisão:** manter uma aplicação modular, com HTTP, consumidor SQS e workers no mesmo executável, usando os mesmos casos de uso e o mesmo PostgreSQL.

O desafio exige processamento distribuído e várias instâncias independentes. A implementação atende esse modelo executando múltiplas cópias da aplicação. A separação em serviços com deploys independentes não foi necessária para implementar as garantias financeiras. Essa escolha mantém as fronteiras de commit e as regras de negócio concentradas, facilitando o estudo e a recuperação de falhas.

| Local | Responsabilidade |
| --- | --- |
| `cmd/api` | Iniciar a aplicação |
| `cmd/sqs-setup` | Provisionar filas e permissões antes da API |
| `internal/domain` | Money, Wallet, WagerTransaction, ledger e invariantes em memória |
| `internal/application` | Casos de uso, autorização por ator, transações, idempotência e eventos |
| `internal/postgres` | SQL, repositórios, constraints e reservas dos workers |
| `internal/httpapi` | Contratos HTTP, validação de entrada e autenticação OIDC |
| `internal/sqsconsumer` | Recebimento SQS, validação de envelopes e publicação de eventos |
| `internal/workers` | Coordenação de publicação e retomada de pendências |
| `internal/brokerauth` | Políticas de acesso às filas e identidades locais |
| `internal/bootstrap` | Configuração, montagem e ciclo de vida com Fx |
| `internal/observability` | Métricas Prometheus |
| `internal/systemtest` | Validação com processos e dependências reais |
| `migrations` | Evolução versionada do schema |

O domínio não importa Fx, HTTP, AWS ou PostgreSQL. Os casos de uso dependem de interfaces e recebem um `Actor` construído pelo adaptador confiável. Os adaptadores compartilham `ProcessWagerInput` e a mesma lógica financeira. Não há repositório de produção em memória.

## 2. Dinheiro, encapsulamento e datas

**Decisão:** representar `Money` como `int64` em unidades mínimas, com moeda e escala fixa de duas casas. Assim, `25.00 BRL` corresponde a `2500` unidades mínimas. Parsing, cálculos, hash e serialização financeira não passam por ponto flutuante.

Campos privados impedem alteração direta. Soma, subtração e negação retornam novos valores; acessores e operações rejeitam `Money{}` não inicializado. As moedas suportadas são BRL, USD e EUR, sem conversão cambial. Uma lista explícita evita tratar qualquer código de três letras como uma moeda suportada.

### Contrato e limites

- Entrada externa: `{"amount":"25.00","currency":"BRL"}`, com strings e exatamente duas casas.
- Rejeição de negativos externos, espaços, sinais, zeros à esquerda, notação científica, `NaN`, `Infinity` e escala incorreta. Não há arredondamento nem normalização silenciosa.
- Quantias externas de `0.00` a `92233720368547758.07`; cada operação aplica sua regra para zero.
- Cálculos internos de `-92233720368547758.08` a `92233720368547758.07`, permitindo diferenças negativas de reconciliação.
- Parsing, soma, subtração e negação verificam overflow. Formatação e comparação também funcionam nos extremos de `int64`.
- Aritmética e comparação exigem moedas compatíveis. Os DTOs de entrada mantêm dinheiro como string e chamam o construtor de domínio.

No PostgreSQL, quantias usam `BIGINT` e moedas usam `TEXT`. A reconciliação soma em `NUMERIC`, permitindo totais brutos maiores que `int64`; somente o saldo líquido é convertido para `Money`, com verificação de limite.

Entidades têm construtores, reidratação e métodos de transição explícitos. `Snapshot()` devolve cópias. Reidratar valida o registro sem aplicar novamente efeitos financeiros ou eventos. Erros esperados são classificáveis com `errors.Is`; rejeições de negócio não usam `panic`.

Os casos de uso recebem relógio e gerador de IDs por dependência; os padrões usam o relógio do processo e UUID v4 com `crypto/rand`. Instantes são normalizados para UTC e truncados em microssegundos, acompanhando `TIMESTAMPTZ`. Datas vazias, regressões de atualização e transições temporalmente inválidas são rejeitadas. O instante externo de uma mensagem não substitui o relógio de aplicação da movimentação.

## 3. Carteira, operações e ledger

**Decisão:** a carteira é a raiz do agregado financeiro. Uma alteração de saldo acontece por `Wallet.Credit` ou `Wallet.Debit` e só se torna durável no commit conjunto com operação, ledger e eventos.

Uma carteira é única por `(playerId, currency)`, começa na versão `1` e preserva identidade, jogador, moeda e data de criação. Débito exige saldo suficiente. A versão incrementa uma vez por mudança efetiva de saldo; valores zero não são créditos ou débitos válidos. Overflow de saldo ou versão não é permitido.

| Operação | Regra financeira |
| --- | --- |
| `OPENING` | Abertura interna positiva, com carteira na versão `1` |
| `BET` | Débito positivo, limitado ao saldo |
| `WIN` | Crédito positivo; referência a uma aposta é opcional |
| `LOSS` | Valor zero; conclui sem saldo, versão ou ledger adicionais |
| `REFUND` | Crédito integral de uma `BET` processada |
| `ROLLBACK` | Movimento integral contrário a uma `BET`, `WIN` ou `REFUND` processada |

Na abertura positiva, o caso de uso grava a carteira, `OPENING` processado, crédito no ledger e os dois eventos financeiros no mesmo commit. A carteira já nasce com o saldo inicial: processar `OPENING` não credita novamente. As identidades `opening:<walletId>` e `ledger:opening:<walletId>` são estáveis. Abertura zero cria somente a carteira. HTTP e SQS rejeitam `OPENING` externo.

O ledger é append-only. Cada lançamento registra identidade, carteira, operação, direção, quantia, saldo anterior, saldo posterior e instante. Sua construção valida a equação `balanceAfter = balanceBefore ± money`. O banco exige unicidade por carteira/operação e por carteira/versão, protege a continuidade dos saldos e impede edição, exclusão ou truncamento do ledger por SQL normal. Correções financeiras exigem novas operações e lançamentos.

`LOSS` gera `WagerTransactionProcessed`, sem ledger ou `WalletBalanceChanged`. Operações rejeitadas também não alteram saldo, versão ou ledger.

## 4. Persistência e fronteira transacional

**Decisão:** usar `pgx` com SQL explícito e transações `READ COMMITTED`. Isso torna locks, consultas e constraints visíveis no código.

`Transactor.WithinTransaction` fornece repositórios vinculados ao mesmo `pgx.Tx`. O caso de uso coordena carteira, operação, ledger, inbox quando aplicável e outbox. Os repositórios não confirmam alterações separadamente e não abrem transações aninhadas.

O fluxo financeiro é:

1. Autorizar a identidade, validar a entrada e calcular o hash.
2. Consultar a idempotência; se necessário, bloquear a carteira e consultar novamente.
3. Validar referência e regras de negócio.
4. Alterar as entidades e gravar os registros necessários.
5. Confirmar a transação SQL e só então devolver o resultado ou permitir o ack da mensagem.

Operações síncronas comuns não confirmam um aceite intermediário em `PENDING`. Falhas antes da confirmação revertem as gravações. Se a resposta do commit se perder, o resultado pode ser incerto: o chamador deve repetir a mesma identidade e conteúdo, consultando a idempotência persistente. Um erro de comunicação não prova que houve rollback.

Cancelamento é verificado pelo gerenciador transacional. O rollback de limpeza usa contexto independente com prazo de cinco segundos. Não há publicação externa dentro da transação financeira.

### Garantias impostas no banco

| Estrutura | Proteção |
| --- | --- |
| `wallets` | Unicidade jogador/moeda, saldo não negativo, versão e identidade |
| `wager_transactions` | Unicidade provedor/ID externo e provedor/chave, referências, estados e imutabilidade terminal |
| `wallet_ledger_entries` | Unicidade, imutabilidade, continuidade, direção, quantia e vínculo com a operação |
| `outbox_events` | Envelope imutável, identidade estável e associação aos efeitos financeiros |
| `inbox_messages` | Unicidade consumidor/mensagem, hash, conclusão e origem da operação |
| `reference_jobs` | Agenda durável por operação e reserva com token |

Constraint triggers adiados até o commit verificam que saldo/versão correspondem ao ledger e que operações processadas, rejeitadas ou pendentes de referência têm seus eventos. Processamento com movimento exige ledger e evento de saldo. Essa ordem permite gravar as tabelas em etapas, sem permitir um commit financeiro parcial.

`OPENING` tem campos externos nulos; o schema distingue a origem interna e impede abertura positiva duplicada. Índices e validações também protegem reversões e a relação entre operação, referência e carteira.

`pgx.ErrNoRows` vira `ErrNotFound`; unicidade, serialização e deadlock são identificáveis como `ErrConflict`. Outros erros não são convertidos automaticamente em rejeição financeira. HTTP responde conflito quando aplicável; não existe um loop geral de retry SQL dentro do caso de uso.

As migrations `000001_initial`, `000002_inbox` e `000003_workers` cobrem essas estruturas. Aplicação e reversão são executadas pelo serviço `migrate` e estão documentadas no README. Reversões removem os dados da versão correspondente.

## 5. Idempotência financeira

**Decisão:** manter a identidade e o resultado no PostgreSQL, com escopo por provedor. Cache em memória e deduplicação FIFO não constituem a garantia financeira.

A chave recebida é preservada. O servidor não a substitui por uma chave derivada. Existem unicidades independentes para `(providerId, idempotencyKey)` e `(providerId, externalTransactionId)`.

O hash é SHA-256 em hexadecimal minúsculo sobre o JSON produzido por `encoding/json` com maps de chaves string ordenadas. Todos os valores financeiros são strings. Essa é a canonicalização definida pelo projeto, sem alegar conformidade com outra especificação de JSON canônico.

| Incluído no hash financeiro | Excluído |
| --- | --- |
| `providerId`, `externalTransactionId`, `walletId`, `playerId`, `roundId`, `gameId`, `kind`, `money.amount`, `money.currency` e referência externa quando preenchida | Chave de idempotência, ID interno, relógio, correlação, causa e metadados de transporte |

Textos são validados como UTF-8; dinheiro já tem representação de duas casas e moeda suportada. Referência vazia equivale à ausência desse campo no objeto canônico.

- Mesma chave e mesmo hash: retorna resultado persistido com `idempotentReplay=true`.
- Mesma chave com conteúdo diferente: conflito.
- Mesmo ID externo com outra chave: conflito, mesmo com conteúdo equivalente.
- Operação concluída: devolve o saldo observado no processamento original, mesmo que a carteira tenha mudado depois.
- Operação ainda pendente: consulta o estado; o replay externo não executa novamente a pendência.

HTTP e SQS compartilham esse cálculo e o mesmo processamento financeiro. Um replay não cria novos eventos ou lançamentos.

## 6. Concorrência e locks

**Decisão:** serializar alterações da mesma carteira com `SELECT ... FOR UPDATE`. O `UPDATE` também exige a versão anterior esperada. Carteiras distintas não compartilham um lock global.

A consulta de idempotência é repetida após obter o lock. Em `READ COMMITTED`, ela pode observar o resultado confirmado por quem detinha a carteira anteriormente. Índices únicos continuam protegendo disputas que escapem à consulta preventiva.

Uma carteira muito disputada serializa seus escritores e pode limitar throughput. Esse é o custo escolhido para manter a coordenação financeira simples e explícita. O lock não fica aberto durante chamadas SQS. A independência entre carteiras e o cenário de duas apostas de `80.00` sobre `100.00` são exercitados com três processos reais.

Workers reservam trabalhos com `FOR UPDATE SKIP LOCKED`, sem manter a transação de claim aberta durante processamento ou publicação. Tokens aleatórios de reserva impedem que um proprietário antigo confirme o trabalho após outra instância assumir o lease expirado.

## 7. Estados e referências pendentes

| Origem | Método | Destino | Condição |
| --- | --- | --- | --- |
| `PENDING` | `WaitForReference` | `PENDING_REFERENCE` | Existe referência externa a aguardar |
| `PENDING` ou `PENDING_REFERENCE` | `Process` | `PROCESSED` | Carteira e referência válidas, quando necessária |
| `PENDING` ou `PENDING_REFERENCE` | `Reject` | `REJECTED` | Recusa de negócio com código válido |
| `PENDING` ou `PENDING_REFERENCE` | `Fail` | `FAILED` | Falha permanente comprovada; transição reservada na operação atual |

`PROCESSED`, `REJECTED` e `FAILED` são terminais. O domínio valida as transições e o banco impede alterar o estado terminal. A retomada de uma operação terminal devolve seu resultado sem reaplicar efeitos.

**Decisão:** uma referência ausente ou ainda em `PENDING`/`PENDING_REFERENCE` produz espera durável, sem movimentação financeira. O registro de espera e seu evento são confirmados juntos. Repetir a tentativa enquanto a referência continua indisponível não cria outro evento de espera.

Um trigger agenda todo `PENDING` ou `PENDING_REFERENCE` confirmado em `reference_jobs`; a migration também agenda pendências preexistentes. O worker chama `Resume` com acesso interno, bloqueia a carteira e relê a operação antes de continuar. Falha após o commit financeiro e antes de concluir o job é segura: a próxima instância encontra o resultado terminal.

### Prazo e tentativas

- Padrões: `REFERENCE_TTL=24h` e `REFERENCE_MAX_ATTEMPTS=100`.
- Backoff dos workers: `2, 4, 8, ... 256` segundos, depois cinco minutos; agenda e tentativas são persistidas.
- O TTL é contado desde a criação. No limite ou após ele, uma operação já em `PENDING_REFERENCE` é rejeitada com `REFERENCE_NOT_FOUND`, mesmo se a referência tiver chegado depois do prazo.
- No limite de tentativas, `ResumeFinalReferenceAttempt` ainda processa uma referência disponível e válida antes do TTL. Se continuar ausente ou pendente, rejeita com `REFERENCE_NOT_FOUND`.
- Referência terminal em `REJECTED` ou `FAILED`: rejeita a dependente com `REFERENCE_NOT_PROCESSED`.
- Erros de infraestrutura mantêm retry, inclusive após atingir o contador; não são convertidos em rejeição financeira.

## 8. Reversões e códigos de rejeição

**Decisão:** permitir somente reversões integrais e uma reversão processada direta por referência. A consulta preventiva `HasProcessedReversal` e um índice único no PostgreSQL aplicam essa regra entre instâncias.

Referências são resolvidas por `(providerId, referenceExternalTransactionId)`. Devem concordar em provedor, jogador, carteira, moeda e rodada. Autorreferências e tipos incompatíveis são rejeitados. O `gameId` é persistido, mas a compatibilidade exigida da referência segue os campos enumerados pelo desafio.

| Tipo | Referência válida | Direção |
| --- | --- | --- |
| `WIN` com referência | `BET` processada; prêmio pode ter outro valor | Crédito |
| `REFUND` | `BET` processada, com mesmo valor | Crédito |
| `ROLLBACK` | `BET`, `WIN` ou `REFUND` processado, com mesmo valor | Crédito para desfazer `BET`; débito para desfazer `WIN`/`REFUND` |

Para uma mesma aposta, `REFUND` e `ROLLBACK` disputam a única devolução direta: não podem creditar duas vezes o mesmo débito. Um rollback de `REFUND` desfaz o reembolso e não reabre a possibilidade de outra devolução direta da aposta. Essa é uma interpretação deliberadamente restritiva das combinações permitidas.

| Código estável | Significado |
| --- | --- |
| `INSUFFICIENT_FUNDS` | Aposta sem saldo |
| `REVERSAL_INSUFFICIENT_FUNDS` | Rollback que precisaria debitar mais que o saldo |
| `REFERENCE_NOT_FOUND` | Espera por referência esgotada |
| `REFERENCE_NOT_PROCESSED` | Referência terminou sem processamento bem-sucedido |
| `REFERENCE_MISMATCH` | Referência incompatível ou reversão parcial |
| `ALREADY_REVERSED` | Referência já recebeu reversão processada |
| `INVALID_OPERATION` | Carteira incompatível ou limite de saldo/versão excedido |

Rejeições aceitas como resultados de negócio são persistidas com seu evento e não mudam a carteira. Entradas inválidas antes do aceite retornam erro e não criam um resultado financeiro terminal. Corrigir uma operação terminal exige uma nova operação, sem alterar seu histórico.

### Política de `FAILED`


Uma falha permanente de processamento precisa de evidência de que uma operação válida não pode ser executada, mesmo após recuperar os serviços e repetir a mesma entrada. Quantidade de tentativas, tempo de indisponibilidade ou uma resposta de erro isolada não demonstram essa condição.

| Situação | Tratamento |
| --- | --- |
| PostgreSQL ou SQS indisponível, timeout, conexão interrompida ou cancelamento | Preservar a possibilidade de retry; não chamar `Fail` |
| Deadlock, conflito de serialização ou concorrência | Repetir com a mesma identidade e conteúdo; conferir o resultado persistido |
| Resposta do commit perdida | Resultado incerto: consultar/repetir pela idempotência; nunca presumir falha financeira |
| Saldo insuficiente, referência incompatível ou reversão repetida | `REJECTED`, com o código de negócio correspondente |
| Referência ausente ou ainda pendente | `PENDING_REFERENCE`; ao esgotar a política de espera, `REJECTED/REFERENCE_NOT_FOUND` |
| JSON inválido, identidade não autorizada ou conteúdo inválido no transporte | Erro HTTP ou DLQ de entrada; não criar uma operação `FAILED` para uma mensagem não aceita |
| Falha ao gravar ledger, operação, inbox ou outbox antes do commit | Reverter a transação inteira e permitir retry |
| Falha de publicação, inclusive erro permanente de entrega ou limite de tentativas | Retry/DLQ da outbox; preservar o estado financeiro confirmado |
| Falha de ack após processamento | Reentrega com inbox/idempotência persistentes |
| Credenciais/permissões incorretas, migration ausente, bug ou violação inesperada de integridade | Exigir correção operacional e preservar a possibilidade de retomada; não classificar automaticamente como `FAILED` |

**Decisão para os adaptadores atuais:** não há uma causa de infraestrutura comprovadamente irreversível que autorize uma transição automática para `FAILED`. As operações financeiras dependem do PostgreSQL, e a entrega ao SQS ocorre após o commit pela outbox. Recuperar essas dependências permite continuar o processamento. O método de domínio `Fail` permanece reservado e não é chamado pelos casos de uso ou workers. Isso é uma restrição deliberada da política, e não uma implementação de encerramento automático em `FAILED`.

Uma futura integração só poderá habilitar essa transição para uma causa explicitamente identificada e documentada. Por exemplo, uma dependência obrigatória poderia confirmar a perda irreversível de um recurso exclusivo daquela operação, sem alternativa de recuperação. Uma resposta genérica de erro dessa dependência continuaria insuficiente.

Antes de conectar uma causa assim ao processamento, o fluxo deverá:

1. Confirmar que a operação foi aceita e permanece em `PENDING` ou `PENDING_REFERENCE`, com identidade e conteúdo persistidos e válidos.
2. Demonstrar que não houve efeito financeiro confirmado ou resultado incerto. Uma operação `PROCESSED` é imutável; eventual correção financeira exige outra operação e novos lançamentos.
3. Registrar a causa, a evidência, a correlação e o instante em auditoria durável, junto da transição, sem alterar saldo ou ledger.
4. Bloquear a carteira, reler a operação e persistir atomicamente a conclusão, respeitando o mesmo protocolo de concorrência dos casos de uso existentes. Se a gravação falhar, manter retry.
5. Devolver o mesmo resultado terminal nos replays e comprovar que reinícios e concorrência não produzem movimentação nem novos encerramentos.

Enviar uma mensagem ou um evento para DLQ encerra uma tentativa de entrega. Isso não determina o estado da operação financeira.

## 9. Inbox e consumo SQS

**Decisão:** adicionar deduplicação de transporte à idempotência financeira, usando a inbox na mesma transação SQL do caso de uso.

Uma deduplicação feita somente pelo broker não sobreviveria à expiração de sua janela FIFO. Separar inbox e operação em commits diferentes também permitiria confirmar a mensagem sem seu efeito durável. A inbox resolve essa coordenação local no PostgreSQL; mensagens com novas identidades ainda precisam passar pela idempotência financeira.

O `messageId` durável é o do envelope, não o ID atribuído pelo broker. A unicidade é `(consumerName, messageId)`, com consumidor `wager-transactions-v1`. `INSERT ... ON CONFLICT DO NOTHING` e leitura com lock coordenam entregas concorrentes.

O hash da inbox cobre tipo, identidade, instante normalizado em UTC, hash financeiro, chave e metadados aceitos de correlação/causa. Reutilizar a identidade de mensagem com conteúdo diferente produz conflito, mesmo se algum dado de negócio for equivalente. Mensagens diferentes ainda passam pela idempotência financeira.

Uma inbox só conclui junto do resultado durável: operação processada, rejeitada ou referência pendente persistida. O ack ocorre após o commit. Se houver queda antes do ack, a reentrega consulta inbox/operação e não movimenta novamente.

Envelopes exigem tipo `WagerTransactionRequested`, instante válido e dados aceitos. O consumidor rejeita campos desconhecidos, chaves JSON duplicadas, JSON excessivamente profundo, formato monetário inválido e `OPENING` externo. O grupo FIFO também é validado.

### Transporte e destinos

| Contrato | Definição |
| --- | --- |
| Entrada / DLQ | `wager-transactions.fifo` / `wager-transactions-dlq.fifo` |
| `MessageGroupId` de entrada | SHA-256 do JSON compacto UTF-8 `[providerId,walletId]` |
| `MessageDeduplicationId` de entrada | SHA-256 de `messageId` |
| Visibilidade | 60 segundos |
| Redrive | Cinco recebimentos antes da DLQ |
| Backoff em erro transitório | Visibilidade de 2, 4, 8, 16 e até 30 segundos, conforme contagem limitada pelo consumidor |
| Mensagem inválida ou erro permanente de entrada | Visibilidade de um segundo; redrive nativo do broker |

O processamento em andamento tem contexto de até 20 segundos e o ack tem até cinco segundos. Não há renovação contínua de visibilidade; esses prazos ficam abaixo dos 60 segundos da fila, e reentregas continuam protegidas pela inbox e pela idempotência financeira.

Deduplicação FIFO é uma otimização com janela limitada. Os testes usam novas identidades de deduplicação do broker para provocar recebimentos repetidos do mesmo envelope e exercitar a garantia da aplicação.

## 10. Outbox e publicação

**Decisão:** confirmar o snapshot do evento no banco antes de qualquer envio externo. A entrega é at-least-once e a identidade do evento permanece estável em retries e republicações.

Enviar ao broker dentro do caso de uso criaria uma fronteira em que o evento poderia sair e o commit financeiro falhar. Enviar somente após o commit sem registrar uma outbox permitiria perder o evento em uma queda. A outbox conserva a intenção de publicação no mesmo banco do saldo, usando recuperação e deduplicação em vez de uma transação distribuída entre PostgreSQL e SQS.

| Evento concreto | Origem |
| --- | --- |
| `WagerTransactionProcessed` | Operação processada, incluindo abertura e `LOSS` |
| `WagerTransactionRejected` | Rejeição definitiva de negócio |
| `WagerTransactionPendingReference` | Primeira persistência da espera |
| `WalletBalanceChanged` | Alteração efetiva de saldo, incluindo abertura positiva |

Tipo e versão `1` são definidos na construção. O envelope contém `eventId`, `eventType`, `aggregateId`, `correlationId`, causa opcional, instante UTC e `data` tipado. O evento de saldo carrega carteira, operação, direção, quantia, saldos anterior/posterior e versão. O payload é serializado antes de inserir a outbox e protegido contra alteração no banco.

O publisher reserva um evento por vez usando lease de 60 segundos e token; envia com timeout de dez segundos dentro de uma etapa de até 20 segundos. Confirmações no banco exigem o token atual. Outra instância assume reservas abandonadas.

`OUTBOX_MAX_ATTEMPTS=8` é o orçamento padrão de reservas normais. Erro permanente de entrega reconhecido ou esgotamento desse orçamento encaminha o mesmo envelope à DLQ de eventos. `published_at` só é registrado após confirmação de envio normal, e `dead_lettered_at` após confirmação de envio à DLQ. Falhas de envio à DLQ também mantêm o evento reagendado. O backoff é o mesmo dos workers de referência.

| Contrato de saída | Definição |
| --- | --- |
| Eventos / DLQ | `wallet-events.fifo` / `wallet-events-dlq.fifo` |
| `MessageGroupId` | SHA-256 de `wallet:<walletId>` |
| `MessageDeduplicationId` | SHA-256 de `eventId` |
| Responsabilidade do consumidor externo | Deduplicar por `eventId` e tratar o envelope versionado |

Entre envio e confirmação local, uma queda pode causar republicação. Não há promessa de exactly-once no broker. O processamento concorrente da outbox também não garante que a ordem de envio corresponda à ordem de commit da carteira; consumidores que dependam dessa ordem precisam considerar a versão da carteira e reconciliar seu estado.

## 11. Autenticação e autorização

### HTTP e identidade do provedor

**Decisão:** delegar emissão de tokens ao Keycloak com OAuth 2.0/OIDC e `client_credentials`. A aplicação valida tokens; não cadastra senhas nem emite credenciais próprias.

O modelo atende comunicação entre serviços com identidades provisionadas e permissões explícitas. Keycloak autentica quem chama HTTP. IAM e políticas das filas autorizam as chamadas ao broker; papéis do Keycloak não concedem permissões SQS. Uma integração interna que publique em nome de provedores precisa validar a origem antes de usar sua identidade IAM confiável.

O Compose importa automaticamente o realm `wallet`, clientes e papéis locais. `wallet-service` tem acesso interno; `provider-a` e `provider-b` têm papéis de provedor com claim assinado `provider_id`. Existe também um cliente de teste com token curto para verificar expiração real.

`go-oidc` verifica assinatura RS256, emissor, audiência e expiração, usando JWKS com cache. São exigidos sujeito e `typ=Bearer`. URLs do issuer/JWKS vêm da configuração confiável, nunca de claims não validados. O JWKS usa timeout HTTP de cinco segundos. O issuer local aponta para a URL vista pela máquina, enquanto o JWKS pode usar `keycloak:8080` dentro do Compose, mantendo a validação de emissor.

- Papel `wallet-internal`: cria `InternalActor`; operações de carteira e retomada exigem esse ator.
- Papel `provider`: exige `provider_id` válido e cria `NewProviderActor`.
- Identidades com papéis ambíguos são rejeitadas.
- `providerId` do corpo ou da rota não concede acesso; precisa corresponder à identidade autenticada.
- Consultas e replays verificam o provedor. A identidade interna não envia apostas externas pela rota de processamento.

Na API, consultas de transação também permitem o ator interno para auditoria; essa permissão não é concedida aos provedores. Endpoints de saúde e métricas são públicos no ambiente local.

### Broker e produtores confiáveis

**Decisão:** tratar a fila compartilhada como transporte de serviços internos confiáveis. Não há autenticação Keycloak por mensagem SQS nem entrega direta de credenciais da fila a provedores externos.

O produtor interno precisa autenticar a origem e validar o provedor antes de publicar. Dentro dessa fronteira, o consumidor cria o ator a partir de `providerId` e aplica as mesmas regras de autorização e domínio. O grupo FIFO é roteamento; não é identidade autenticada.

`internal/brokerauth` descobre os ARNs reais e aplica políticas de recurso com permissões específicas e negações explícitas de envio, consumo e administração. O runtime consome solicitações e publica eventos; o publicador confiável envia solicitações; outra identidade consome eventos. As exceções de redrive nativo permitem somente a fila de origem designada. Permissões em lote seguem as ações IAM individuais correspondentes.

No Compose, MiniStack `1.5.30` executa com `AUTH=true`. O setup cria usuários separados, políticas restritas e arquivos privados de credenciais em volumes distintos. A API monta somente suas credenciais em modo de leitura. Credenciais administrativas locais ficam no setup. Em AWS, o deployment cria roles e relações de confiança; o setup exige quatro ARNs válidos e distintos e configura as políticas das filas.

O emulador mantém bypass administrativo root/sem assinatura e não é uma fronteira de segurança de produção. Os testes de broker verificam assinaturas e permissões das identidades restritas criadas. Não foi realizado deployment nem validação de enforcement em uma conta AWS real.

## 12. Composição e ciclo de vida com Uber Fx

**Decisão:** usar Fx somente na montagem da aplicação. Os componentes mantêm construtores Go e não consultam um container global. `cmd/api` chama `bootstrap.New().Run()`.

`fx.Module` agrupa configuração, PostgreSQL, observabilidade, casos de uso, SQS, workers e HTTP. `fx.Provide` registra construtores e `fx.Invoke` conecta hooks. O Fx gerencia recursos por `fx.Lifecycle`, com limites globais de 30 segundos para início e parada.

### Inicialização

1. Validar o grafo e a configuração; erros impedem a inicialização.
2. Abrir o pool PostgreSQL e executar `Ping`, com prazo de até dez segundos dentro do orçamento global.
3. Com SQS habilitado, verificar schema/filas e iniciar o consumidor.
4. Com workers habilitados, verificar estruturas necessárias e iniciar retomada/publicação, conforme configuração.
5. Abrir o listener HTTP sincronamente; falha de bind faz o startup falhar. Erro inesperado do servidor solicita parada com código não zero.

A configuração OIDC é validada na montagem; o JWKS é consultado pela validação de tokens e readiness. O contexto do cache de chaves tem vida própria até o shutdown e não usa o contexto temporário de startup. Migrações e provisionamento não são executados pelo runtime da API: são tarefas anteriores do Compose.

Aquisição de banco ocorre em `OnStart`, permitindo limpeza em falha parcial. O Fx reverte os hooks de componentes já iniciados, e adaptadores limpam recursos adquiridos quando sua própria inicialização falha.

### Shutdown

Hooks de parada executam em ordem inversa. Hooks finais interrompem primeiro novas buscas SQS e novos claims dos workers. Em seguida:

1. O servidor HTTP deixa de aceitar conexões e drena requisições por até 25 segundos; se necessário, fecha as conexões.
2. O contexto OIDC é cancelado após o HTTP. Trabalhos já iniciados usam seus contextos limitados e são aguardados pelos hooks dos workers e do consumidor; clientes são fechados conforme seus ciclos de vida.
3. Mensagens recebidas, mas ainda não iniciadas, têm visibilidade liberada; trabalho interrompido sem ack fica disponível após o timeout do broker.
4. O pool PostgreSQL fecha depois dos componentes que o utilizam.

O orçamento global de 30 segundos é compartilhado pelos hooks; os prazos individuais não são somados como garantia de tempo adicional. O Compose concede 35 segundos antes de matar o container. Se houver interrupção forçada, inbox, idempotência, outbox e leases sustentam a recuperação posterior.

## 13. Reconciliação, HTTP e observabilidade

**Reconciliação:** bloqueia a carteira e compara o saldo armazenado à soma exata do ledger, incluindo abertura. Escritores usam o mesmo lock, garantindo uma visão consistente. Retorna `difference = storedBalance - calculatedBalance`, consistência e quantidade de lançamentos. Divergências geram log e métrica, sem alterar saldo ou histórico.

**Paginação:** ledger ordenado por `(createdAt,id)`, com cursor Base64 URL contendo versão, carteira e posição. O cursor é opaco no contrato, vinculado à carteira e não criptografado. Limite padrão `50`, máximo `200`.

**HTTP:** `net/http`, DTOs explícitos, corpo limitado a 1 MiB e contexto de requisição de 20 segundos. Entrada inválida usa `400`, credenciais `401`, proibição `403`, ausência `404`, conflito `409`, pendência `202` e resultado financeiro rejeitado/falha terminal `422`. Erros que impedem confirmar processamento usam `503` e orientam retry com mesma chave/conteúdo. Consultas autorizadas usam `200` para qualquer estado encontrado. Os contratos executáveis estão no README.

### Logs e correlação

O logger JSON é compartilhado com os eventos Fx. Logs usam identificadores disponíveis, sem tokens, secrets, payloads financeiros completos ou erros brutos que possam expor parâmetros de conexão.

- HTTP preserva ou gera `correlationId` e registra rota, status e duração; processamento registra operação, carteira, provedor e replay.
- SQS registra `messageId` do envelope, ID do broker, correlação e operação; confirmação e erro de ack também incluem causa e identidades financeiras disponíveis.
- Outbox registra worker, evento, agregado, tentativa e destino. Extrai correlação, causa, operação, carteira, provedor e identidade externa do envelope quando presentes. Payload inválido mantém os identificadores do job no log, sem alterar a política de entrega.
- Referências registram operação, correlação, causa e tentativa, inclusive em falhas. Após obter resultado, acrescentam carteira, provedor, identidade externa e estado.

A retomada usa `correlationId=transactionId` e causa `reference-worker:<transactionId>`. Não preserva automaticamente uma correlação externa personalizada do recebimento: use o `transactionId` para atravessar essa fronteira. Esses valores seguem nos novos eventos; a publicação conserva os identificadores persistidos. Erros anteriores à reserva identificam o worker, pois ainda não há trabalho específico disponível.

### Métricas e saúde

Prometheus expõe `/metrics`, com resultados por estado/replay, retries, DLQ, conflitos, latências, backlog/idade da outbox e reconciliações consistentes/divergentes. O domínio e os casos de uso recebem interfaces de observação, sem importar Prometheus.

Resultados são observados após commit. Contadores são locais por instância e podem reiniciar; gauges do banco representam estado compartilhado e não devem ser somados entre réplicas. Labels não incluem jogadores ou operações. Profundidades de DLQ são aproximadas. Falha de coleta durável aparece em `wallet_storage_metrics_up`.

`/health/live` indica que o processo responde. `/health/ready` executa checks independentes em paralelo, com orçamento de dois segundos, cobrindo PostgreSQL, estruturas do schema, IdP/JWKS, consumidor e workers habilitados. Falhas produzem `503`. Modos parciais explicitam componentes desabilitados; desabilitar SQS não remove os outros checks configurados.

Readiness não reconcilia todas as carteiras, não verifica integralmente cada constraint do schema e não prova permissões IAM de escrita. Esses aspectos dependem de migrations, testes, reconciliação e configuração operacional.

## 14. Verificação e alcance da evidência

| Suíte | O que verifica |
| --- | --- |
| `internal/domain` e `internal/application` | Precisão, overflow, invariantes, regras, estados, abertura, referências, reversões, hash e rollback transacional por falhas injetadas |
| `internal/postgres`, tag `integration` | Banco real, constraints, imutabilidade, atomicidade, replay persistente, inbox, fencing de leases e reversão/reaplicação das migrations |
| `internal/httpapi`, tag `integration` | PostgreSQL e Keycloak reais, autenticação, permissões, isolamento, token expirado, contratos e ausência de efeitos indevidos |
| `internal/systemtest`, tag `integration` | Três processos reais, concorrência HTTP/SQS, reentregas, publishers, interrupções, dependências indisponíveis, Fx e consistência final |
| `TestSQSBrokerAuthorization` | Broker real local, identidades separadas, permissões, recusas, lotes e assinaturas |

A suíte distribuída envia 50 requisições HTTP concorrentes da mesma aposta e provoca recebimentos SQS repetidos. Exercita duas apostas de `80.00` sobre saldo `100.00`, carteiras independentes, reinícios e referências resolvidas ou expiradas. Interrompe após commit e antes de ack, e antes/depois do envio de eventos, verificando recuperação sem novos efeitos financeiros.

Também cobre retries/DLQ, falhas de dependências, readiness, divergência de reconciliação, falha parcial de startup e comparação final dos saldos com o ledger. Proxies de teste controlam fronteiras de transporte; o executável de produção não possui flags de injeção de falha.

A execução completa apresentada em 22/10/2026 aprovou 388 testes e subtestes, sem falhas, testes ignorados ou alertas de condições de corrida. Usou PostgreSQL, Keycloak e MiniStack reais e três processos da API; a conferência final encontrou saldos iguais ao ledger e nenhuma movimentação duplicada. O script também executa `go vet` com a tag de integração e remove a infraestrutura temporária.

Durante a validação anterior, um cenário de interrupção do consumidor expirou antes de atingir a fronteira de ack. A espera desse cenário foi ajustada para cobrir a visibilidade real da fila, além do prazo de preparação, preservando suas verificações de commit, reentrega e ausência de duplicidade. As execuções completas posteriores foram aprovadas.

Essa evidência corresponde às execuções observadas; alterações futuras exigem nova validação. Esta edição modifica somente documentação e não executa novamente a suíte. Os comandos reproduzíveis e o roteiro manual estão no README; o executor está em [scripts/test-system.sh](scripts/test-system.sh).

Limites da evidência:

- O broker é MiniStack, sem substituir uma validação em AWS real.
- O teste de recuperação de lease acelera a expiração no banco descartável; o teste de reentrega SQS espera a visibilidade real.
- Corrupção administrativa para testar reconciliação é temporária, isolada e restaurada antes da conferência final.
- Cenários controlados não demonstram recuperação de toda combinação possível de falhas nem uma meta de desempenho em produção.

## 15. Limitações, interpretações e trabalho não concluído

### 15.1. Interpretações adotadas

- **Distribuição:** três ou mais instâncias independentes da mesma aplicação satisfazem o modelo de concorrência do desafio. A solução mantém HTTP, consumidor e workers no mesmo executável e não exige microserviços separados.
- **Dinheiro:** a escala do contrato é fixa em duas casas para as moedas explicitamente suportadas. Formas monetárias alternativas são rejeitadas antes do hash, sem arredondamento.
- **Reversões:** uma referência só admite uma reversão direta processada. REFUND e ROLLBACK sobre a mesma aposta disputam a mesma devolução; desfazer um reembolso não reabre essa possibilidade.
- **Mensageria:** o produtor da fila é um serviço interno confiável. Provedores externos usam HTTP autenticado; o corpo de uma mensagem não é uma credencial de provedor.
- **Falhas:** esgotar tentativas de entrega encaminha para DLQ; não transforma uma operação financeira processada em `FAILED`. Falhas transitórias de infraestrutura continuam recuperáveis. `FAILED` exige uma causa permanente concreta antes de habilitar a transição no processamento.

### 15.2. Limitações da implementação atual

| Tema | Situação e consequência |
| --- | --- |
| Distribuição | Múltiplos processos da mesma aplicação; não há decomposição em microserviços com deploy independente |
| Moedas | BRL, USD e EUR com duas casas; outras moedas e câmbio não são suportados |
| Concorrência | Serialização por carteira; não há benchmark de saturação de uma carteira muito disputada |
| Reversões | Integrais e uma reversão direta processada por referência; rollback de reembolso não reabre a aposta |
| `FAILED` | Domínio e schema suportam o estado; falta uma causa irreversível concreta e o fluxo auditável antes de habilitar sua produção automática |
| Entrada de provedores | HTTP autenticado; fila interna exige produtor confiável. Não há uma integração genérica implementada para autenticar produtores externos que publiquem diretamente |
| AWS | Roles, trust policies e deployment são externos; não houve validação em conta AWS real |
| Configuração local | Keycloak em modo de desenvolvimento, credenciais de fixtures e broker com bypass administrativo; requer configuração de produção própria |
| Banco local | Compose usa o usuário provisionado do PostgreSQL também no runtime; separar proprietário de migrations e usuário SQL restrito exige configuração adicional |
| Publicação | At-least-once; consumidor externo deve deduplicar. Ordem de envio não é garantida pela ordem de commit |
| Operação da DLQ | Encaminhamento implementado; não há um serviço de reprocessamento automático da DLQ |
| Correlação | A retomada gera sua correlação a partir da operação; a correlação externa original é ligada via `transactionId` |
| Operação prolongada | Não há renovação contínua de lease/visibilidade; os prazos atuais limitam o trabalho e a recuperação depende das reservas duráveis |
| Crescimento dos dados | Não há política automatizada de retenção/arquivamento de ledger, inbox e outbox |
| Observabilidade | Métricas e logs implementados; tracing, dashboards e alertas operacionais não foram entregues |
| Diferenciais opcionais | Sem partidas dobradas, testes de carga ou relatório de throughput/p50/p95/p99 |

### 15.3. Trabalho não concluído e extensões

| Grupo | Trabalho ainda não realizado | Relação com a entrega atual |
| --- | --- | --- |
| Política reservada de `FAILED` | Identificar uma causa irreversível real e conectar seu encerramento à auditoria durável e aos casos de uso | Limitação explícita do runtime: domínio e schema validam o estado, mas nenhum fluxo atual o produz automaticamente |
| Deployment em AWS | Provisionar roles/trust policies e validar as políticas, reentrega e recuperação na conta de destino | A evidência atual usa o emulador local permitido pelo desafio |
| Configuração de produção | Configurar TLS, segredos gerenciados e separar usuário SQL de runtime do proprietário de migrations | O Compose é um ambiente de desenvolvimento com credenciais de exemplo |
| Operação dos dados e das DLQs | Definir retenção/arquivamento e um processo de inspeção e reprocessamento de DLQ | Entrega à DLQ e preservação dos registros estão implementadas; não há automação completa de manutenção |
| Evolução dos prazos e da correlação | Se os trabalhos passarem a exceder os limites atuais, implementar renovação de reservas/visibilidade; se exigido, persistir a correlação original para a retomada | Hoje a execução é limitada por contexto e a ligação entre etapas usa também `transactionId` |
| Diferenciais opcionais | Tracing, dashboards, alertas, partidas dobradas e testes de carga com metodologia e percentis | Não foram entregues; não são necessários para afirmar os resultados dos testes funcionais executados |

Ampliação das moedas, separação em microserviços e mudanças na estratégia de locks dependem de requisitos novos ou de evidência de necessidade operacional.

Esses pontos delimitam o alcance da solução. Decisões de produção, causas futuras de `FAILED` e novos diferenciais exigem implementação e evidência próprias; não são garantias inferidas dos testes locais.
