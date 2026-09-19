---
status: complete
created_at: 2026-09-19T08:50:11Z
requester: user
repository: spynel
git_commit: 91c6befdbd9ed1e7de7a885ebfd10702dc4c5a03
branch: main
topic: "Spynel architecture, security, and quality-gate assessment"
tags:
  - research
  - architecture
  - security
  - quality-gates
  - orchestration
---

# Ricerca sull'architettura, la sicurezza e i gate di qualità di Spynel

## Ambito

Questa ricerca documenta l'analisi statica della codebase Spynel con quattro obiettivi:

- descrivere il funzionamento dell'applicazione e i suoi confini di fiducia;
- distinguere i controlli deterministici dalle istruzioni affidate agli agenti;
- registrare i findings di sicurezza emersi dalla lettura del codice;
- stimare quanto sia semplice modificare le logiche interne, in particolare i gate di review e qualità.

L'analisi copre il commit `91c6befdbd9ed1e7de7a885ebfd10702dc4c5a03` sul branch `main`.

Non sono stati eseguiti pentest, exploit, scansioni di servizi esterni o prove con credenziali. I findings di sicurezza sono quindi classificati come comportamenti verificati nel sorgente, limiti di design oppure candidati da riprodurre in un ambiente isolato. Nessun candidato viene presentato come vulnerabilità dinamicamente confermata.

All'avvio dell'indagine il repository non conteneva `substrate/directives/`, `substrate/expectations/` o tracce Mycelium precedenti. Non risultavano quindi direttive DRC, expectation EXP o decisioni pregresse da integrare in questa valutazione.

## Sintesi

Spynel è un programma Go senza AI incorporata. Coordina canali di comunicazione, processi harness esterni, sessioni, stato Markdown, notifiche e recovery. Un solo processo primario per workspace possiede l'application service, i canali remoti e l'orchestrator; TUI e CLI raggiungono quel processo tramite un'API locale autenticata.

Il control plane è progettato con attenzione. L'API locale autentica tutte le route, l'elezione del primario usa lease atomiche e token per termine, le allow-list remote falliscono in chiusura, i processi vengono avviati senza shell interpolation e updater e installer applicano controlli su checksum, path e tipi di archivio.

Il limite principale è il modello di fiducia del workspace. Lo stato operativo, le configurazioni, i prompt, le estensioni e le lease vivono tutti sotto `.spynel/`, dentro il workspace. La configurazione predefinita del harness è `danger-full-access`. Un harness o repository malevolo che può scrivere nel workspace può quindi alterare anche i meccanismi che dovrebbero controllarlo.

I gate di qualità sono utili contro errori e agenti cooperativi. Non sono un confine di sicurezza contro codice locale ostile. La review può essere forzata deterministicamente con `harness.reviews: always`, ma un attore che modifica lease e cartelle terminali può evitare la riconciliazione che applica il gate.

I problemi con priorità maggiore sono:

- il `PATH` dell'utente viene conservato durante la disinstallazione elevata con sudo;
- un repository può preinserire estensioni eseguibili senza una prova di installazione esplicita;
- un repository può fornire una configurazione custom ACP che avvia un comando locale;
- i gate di workflow dipendono da lease e stato collocati nello stesso albero scrivibile dal harness;
- le allow-list Telegram basate su username ereditano il rischio di trasferimento dello username.

## Metodo e livelli di certezza

La ricerca usa tre livelli di certezza:

- **Comportamento verificato nel sorgente**: il percorso di esecuzione è stato letto direttamente e sono indicati file, simboli e linee.
- **Limite di design**: il codice funziona come progettato, ma la garanzia risultante è più debole di quanto un lettore possa assumere.
- **Candidato di sicurezza**: il percorso appare sfruttabile nel modello di minaccia indicato, ma non è stato riprodotto dinamicamente in questa attività.

L'analisi è stata incrociata con locator, analyzer, pattern finder e più security review specialist. Un ulteriore passaggio con solution architect non è stato disponibile per limite del provider. I riferimenti finali sono stati verificati nuovamente contro i file del repository.

## Architettura

### Composizione

`cmd/spynel` contiene solo il punto di ingresso. La composizione concreta avviene sotto `internal/cli`, che costruisce:

- il configuration store;
- il runtime log;
- il harness supervisor;
- l'application service;
- il channel supervisor;
- l'orchestrator;
- l'API locale;
- la primary election.

I confini principali sono descritti anche in `docs/architecture.md` e in `internal/AGENTS.md`.

### Processo primario

Ogni processo associato allo stesso workspace partecipa all'elezione in `internal/instance/election.go`.

La lease primaria contiene:

- instance ID;
- PID;
- endpoint locale;
- environment ID;
- bearer token;
- data di avvio e heartbeat;
- eventuale destinazione di handoff.

I parametri correnti sono definiti in `internal/instance/election.go:21-27`: heartbeat ogni cinque secondi, stale threshold di trenta secondi e handoff timeout di dieci secondi.

`TryAcquire`, `Renew`, `Handoff` e `Release` sono rispettivamente in `internal/instance/election.go:171`, `:209`, `:247` e `:288`. Le mutazioni passano da un lock cross-processo, mentre la lease viene pubblicata con sostituzione atomica.

Il processo primario possiede:

- application service;
- harness supervisor;
- orchestrator continuo;
- Telegram e WhatsApp;
- API locale;
- scheduler semantico e recovery delle conversazioni.

I processi secondari leggono la lease e usano l'endpoint autenticato. L'environment ID deriva da un token privato per utente e ambiente, così un client può rifiutare una lease nota come appartenente a un host o container diverso.

### API locale

`internal/localapi/server.go:84-106` registra ogni route attraverso `s.authorize`. Il wrapper in `internal/localapi/server.go:213-220` confronta l'header bearer con `subtle.ConstantTimeCompare`.

Le richieste JSON hanno un limite di un MiB, devono essere UTF-8 valide e vengono decodificate con `DisallowUnknownFields` in `internal/localapi/server.go:425`.

Questa API è il control plane comune per TUI e CLI. Anche il TUI ospitato dal processo primario passa attraverso il servizio locale, evitando due proprietari distinti dello stato mutabile.

### Canali remoti

Telegram e WhatsApp traducono eventi esterni in `core.Message` e non conoscono direttamente l'implementazione del harness.

La configurazione impedisce di abilitare un canale senza una allow-list valida. Gli adapter ripetono i controlli nelle operazioni inbound e outbound. Telegram verifica inoltre il secret del webhook in tempo costante in `internal/channel/telegram/telegram.go:327`.

Il channel supervisor sostituisce soltanto gli adapter la cui fingerprint di configurazione è cambiata. Prima della sostituzione revoca l'autorità dell'istanza precedente.

### Application service

`internal/app/service.go` gestisce l'ammissione dei messaggi, la history, i comandi, gli hook, la costruzione dei prompt e il dispatch al harness.

Il percorso ordinario è:

1. validazione e deduplicazione del messaggio;
2. hook `message.received`;
3. persistenza nella history;
4. comando framework oppure costruzione del prompt;
5. hook `harness.before`;
6. aggiunta delle istruzioni framework e delle istruzioni persistenti del ruolo;
7. dispatch al harness;
8. raccolta e persistenza degli eventi;
9. hook `harness.after`;
10. consegna della risposta terminale.

I punti di hook si trovano in `internal/app/service.go:558`, `:597` e `:787`.

### Harness

Gli adapter provider-specifici vivono in `internal/harness`. Il resto dell'applicazione dipende dall'interfaccia comune.

Il supervisor gestisce:

- sessioni indipendenti;
- steering nativo o follow-up in coda;
- snapshot atomici di modello, effort e service mode;
- interruzione e rilascio del provider;
- sostituzione del harness soltanto quando inattivo.

I processi vengono avviati con `exec.CommandContext` e vettori di argomenti. Le opzioni custom ACP non passano attraverso una shell.

### Stato Markdown e orchestrator

Task e goal sono documenti Markdown con front matter YAML. Le route sono fisse in `internal/orchestrator/routes.go`.

Il task lifecycle principale è:

```text
todo -> working -> review -> reviewing -> done
```

Il goal lifecycle principale è:

```text
proposed -> planning -> active -> review -> reviewing -> done
```

La review può anche riportare il goal in `planning` oppure concluderlo in `waiting` o `abandoned`.

`internal/orchestrator/manager.go:445` esegue una scansione ordinata che:

1. ripara claim interrotti;
2. riconcilia le transizioni osservate;
3. recupera lease stale;
4. recupera documenti claimed senza lease;
5. risveglia documenti waiting;
6. avanza goal attivi;
7. effettua nuovi claim;
8. processa l'outbox.

Le lease separano le fasi, impediscono claim duplicati e conservano owner, sessione, heartbeat e attempt. Il documento Markdown resta però la fonte durevole dello stato funzionale.

## Gate di qualità

### Controlli deterministici

I controlli seguenti sono applicati dal codice e non dipendono soltanto dal testo del prompt:

- `TaskPolicyFromDocument` in `internal/orchestrator/markdown.go:28` interpreta `review_required` mancante o malformato come `true`;
- `EffectiveTaskReviewRequired` in `internal/config/config.go:85` applica l'overlay globale `skip-trivial`, `always` o `never`;
- `reconcileTaskTransition` in `internal/orchestrator/manager.go:912` devia le transizioni task non ammesse;
- la modalità effettiva viene calcolata in `internal/orchestrator/manager.go:952`;
- la completion diretta passa da `validateDirectCompletionEvidence` in `internal/orchestrator/notification_message.go:188`;
- il reviewer task non può accettare `done` quando riusa il thread dell'implementatore, se il thread è disponibile e registrato;
- `goalReviewProvesDone` in `internal/orchestrator/workflow.go:108` richiede un `last_review` coerente con il round corrente;
- `harness.reviews: never` non disabilita la review finale dei goal;
- transizioni, hook terminali e notifiche vengono applicati durante la riconciliazione della lease.

### Controlli affidati agli agenti

Spynel non può verificare semanticamente:

- se un task è davvero triviale;
- se un comando di test dichiarato nell'evidence è stato eseguito;
- se il risultato di quel test è stato riportato correttamente;
- se una review è stata sufficientemente approfondita;
- se un reminder è utile;
- se un reviewer provider-side ha mantenuto una separazione cognitiva reale.

`validateDirectCompletionEvidence` controlla struttura, campi obbligatori e timestamp. Non controlla la verità dell'evidence.

I file `.spynel/prompts/*.md`, `.spynel/instructions/*.md` e gli `AGENTS.md` del workspace guidano il comportamento degli agenti. Le istruzioni framework più importanti vengono aggiunte fuori dai template modificabili, ma restano prompt, non policy isolate dal processo provider.

### Modalità disponibili

La modalità può essere cambiata senza ricompilare:

```text
/config set harness.reviews always
/config set harness.reviews skip-trivial
/config set harness.reviews never
```

- `always` forza ogni task nel percorso di review indipendente;
- `skip-trivial` conserva la scelta per documento;
- `never` forza la completion diretta con evidence;
- nessuna modalità rimuove la review finale dei goal.

## Findings di sicurezza

### Estensioni preinserite nel workspace

**Classificazione:** candidato di sicurezza, priorità alta nel modello di repository non fidato.

**Comportamento verificato nel sorgente:**

- le estensioni sono abilitate di default in `internal/config/config.go:225`;
- `Runner.discover` scansiona direttamente la directory configurata in `internal/extensions/hooks.go:160`;
- il manifest può dichiarare comandi per gli hook supportati;
- il comando viene avviato in `internal/extensions/hooks.go:110` con la directory dell'estensione come working directory;
- l'ambiente del processo viene ereditato in `internal/extensions/hooks.go:113`;
- gli hook possono intercettare messaggi e prompt in `internal/app/service.go:558` e `:597`.

Non esiste un registro di installazione che distingua un'estensione installata esplicitamente con `/extension install` da una directory già presente nel repository.

**Scenario:** un repository contiene `.spynel/extensions/<nome>/.spynel-extension.yaml`. Dopo l'inizializzazione o il caricamento del workspace, il primo evento compatibile può eseguire il comando dichiarato con i privilegi dell'utente.

**Impatto:** esecuzione di codice locale, accesso all'ambiente del processo, modifica dei prompt e cancellazione di operazioni. Il sandbox del harness non media gli hook.

**Nota di threat model:** le estensioni installate sono codice fidato per design. Il finding riguarda il consenso implicito dato dalla sola presenza sul filesystem.

### Comando custom ACP fornito dal repository

**Classificazione:** candidato di sicurezza, priorità alta nel modello di repository non fidato.

**Comportamento verificato nel sorgente:**

- `config.Find` adotta la `.spynel/config.yaml` trovata risalendo il workspace;
- `ResolveConfiguredCommand` in `internal/harness/catalog.go:207` risolve il comando configurato;
- `Service.Start` in `internal/app/service.go:400` avvia il supervisor;
- `ACP.Start` in `internal/harness/acp.go:136` costruisce il processo;
- `internal/harness/acp.go:152` esegue il comando configurato con gli argomenti salvati.

**Scenario:** un repository include una configurazione valida con `harness.name: acp` e `harness.acp_command` diretto a un eseguibile del repository.

**Impatto:** esecuzione di codice locale all'avvio del servizio, prima che una conversazione ordinaria produca un risultato.

**Nota di threat model:** il custom ACP è una funzionalità intenzionale. Il problema è l'assenza di una conferma alla prima adozione di una configurazione fornita dal repository.

### PATH non fidato durante la disinstallazione elevata

**Classificazione:** candidato di sicurezza, priorità immediata, impatto potenzialmente root.

**Comportamento verificato nel sorgente:**

- `internal/cli/uninstall.go:76` rilancia il comando con `sudo env` mantenendo `PATH=` derivato da `os.Getenv("PATH")`;
- `internal/updater/uninstall.go:80` esegue `npm uninstall` usando il nome `npm`, quindi la risoluzione dipende dal PATH del processo elevato.

**Scenario:** un eseguibile `npm` controllato dall'utente precede il vero npm nel PATH. L'utente avvia una disinstallazione che richiede privilegi e approva la richiesta sudo.

**Impatto:** il binario controllato dall'utente può essere eseguito come root.

**Correzione attesa:** risolvere e verificare i path degli helper prima dell'elevazione oppure usare nel processo elevato un PATH composto soltanto da directory di sistema fidate.

### Allow-list Telegram basata su username

**Classificazione:** candidato di sicurezza, priorità media.

**Comportamento verificato nel sorgente:**

- `Bot.allowed` in `internal/channel/telegram/telegram.go:626` autorizza tramite numeric ID oppure username normalizzato;
- dopo un messaggio privato autorizzato, `RecordVerifiedPrivate` viene chiamato in `internal/channel/telegram/telegram.go:390`;
- `IdentityStore.AuthorizedPrivate` in `internal/channel/telegram/identity.go:78` usa lo username registrato finché quello username resta nella allow-list.

**Scenario:** un operatore autorizza uno username. Il proprietario originale lo libera e un altro account lo acquisisce.

**Impatto:** il nuovo proprietario può superare l'autorizzazione inbound e diventare una destinazione valida per conversazioni e notifiche collegate a quello username.

**Mitigazione immediata:** usare numeric Telegram user ID nelle allow-list.

### Bypass della review tramite rimozione della lease

**Classificazione:** candidato di integrità del workflow, priorità alta se il harness è considerato non fidato.

**Comportamento verificato nel sorgente:**

- `reconcileTransitions` in `internal/orchestrator/manager.go:842` parte dalle lease esistenti;
- `recoverOrphanClaims` in `internal/orchestrator/manager.go:1187` recupera documenti nelle cartelle claimed come `working`, `reviewing` e `planning`;
- non esiste nello stesso percorso una scansione generale dei documenti appena comparsi in `done` senza lease;
- i file di lease e le cartelle task vivono sotto `.spynel/` nello stesso workspace.

**Scenario ipotizzato:** un harness con accesso in scrittura elimina la propria lease e sposta direttamente un documento in `done`.

**Impatto:** la transizione non entra nel percorso che applica `EffectiveTaskReviewRequired`, gli hook terminali e le notifiche. Il bypass può quindi evitare anche `harness.reviews: always`.

**Nota di threat model:** con accesso completo al workspace, lo stesso attore può già alterare molti altri file. Questo finding dimostra che i gate proteggono la coerenza del workflow, non isolano il workflow dal provider.

### Completion diretta autocertificata

**Classificazione:** limite di design, priorità media.

In `skip-trivial`, la policy viene letta dal documento durante `reconcileTaskTransition`. Un implementatore che può modificare il documento può impostare `review_required: false` e fornire un `completion_summary` formalmente valido.

Il controllo in `internal/orchestrator/notification_message.go:188` richiede evidence, uncertainty e timestamp coerenti. Non può verificare che l'evidence sia autentica.

`harness.reviews: always` evita questo percorso finché lease e stato non vengono alterati.

### Lease primaria malformata

**Classificazione:** comportamento verificato nel sorgente, impatto di disponibilità con priorità bassa.

`TryAcquire` in `internal/instance/election.go:171` tratta una lease non decodificabile come non rinnovabile e azzera `current` in `internal/instance/election.go:183`.

Un attore che può scrivere `primary.json` mentre il primario è vivo può causare un takeover immediato inserendo una lease malformata oppure una lease formalmente valida con heartbeat già stale. Il primario precedente rileverà la perdita al rinnovo successivo, ma può esistere una breve finestra con due owner attivi.

Il prerequisito è già forte: accesso in scrittura allo stato runtime dello stesso workspace.

### Indipendenza della goal review

**Classificazione:** limite di design, priorità bassa o media secondo il valore dei goal.

`reconcileGoalTransition` in `internal/orchestrator/manager.go:1024` e `goalReviewProvesDone` in `internal/orchestrator/workflow.go:108` applicano i requisiti strutturali del verdict. Non compare un controllo equivalente al confronto fra thread di implementazione e review usato per i task.

Le session key di planning e goal review sono distinte, ma l'indipendenza effettiva dipende dal comportamento corretto dell'adapter e del provider.

### Lease hook_cancelled senza recovery

**Classificazione:** comportamento verificato nel sorgente, impatto di disponibilità con priorità bassa.

Quando l'hook `task.claimed` cancella l'operazione, `internal/orchestrator/manager.go:602` imposta `lease.State = "hook_cancelled"`. `recoverStale` salta esplicitamente queste lease in `internal/orchestrator/manager.go:1478`.

Un'estensione fidata che cancella un claim può quindi lasciare il task fermo finché un operatore non interviene sullo stato.

## Controlli di sicurezza efficaci

### Autenticazione locale

- Tutte le route HTTP locali usano lo stesso wrapper di autenticazione.
- Il token cambia per ogni ownership term.
- Il confronto del bearer token è in tempo costante.
- Il client rifiuta environment ID noti come estranei.
- Il socket Unix opzionale applica controlli su owner, modo e descriptor.

### Autorizzazione dei canali

- Telegram e WhatsApp richiedono allow-list valide prima dell'avvio.
- Gli adapter ripetono i controlli sulle operazioni utili.
- Telegram webhook richiede un secret.
- La revoca blocca il traffico utile e consente soltanto il teardown necessario.

### Processi ed estensioni

- Gli argomenti vengono passati direttamente a `exec.CommandContext`.
- I custom ACP args sono vettori, non testo valutato da una shell.
- stdout e stderr degli hook hanno limiti espliciti.
- I nomi degli hook sono allow-listed.

### Filesystem e updater

- Le scritture durevoli usano pubblicazione atomica tramite `internal/fsx`.
- Molti confini sensibili rifiutano symlink e file non regolari.
- Gli archivi di release vengono controllati per checksum, traversal, link, tipi speciali, duplicati e dimensioni.
- L'updater verifica la proprietà dell'installazione prima della rimozione.
- Il signaling dei processi usa identità ed executable path invece del solo nome del processo.

### Limiti e redazione

- API, log, hook, job archive, history e media hanno limiti di dimensione.
- I log passano da un confine centralizzato che rimuove controlli e forme comuni di credenziali.
- Le proiezioni pubbliche di job e status omettono prompt, conversation ID e dati arbitrari.

## Qualità dello sviluppo e della release

`scripts/dev.sh` espone:

- build dell'eseguibile;
- `go test` del modulo e del fork Bubble Tea;
- `go vet`;
- controllo DOX.

`scripts/smoke.sh` costruisce l'applicazione e verifica inizializzazione, documentazione, istruzioni, task, goal, status, conversazioni e comandi principali.

`.github/workflows/release.yml` esegue:

- `go test` in `.github/workflows/release.yml:45`;
- `go vet` in `.github/workflows/release.yml:46`;
- build in `.github/workflows/release.yml:48`;
- smoke test in `.github/workflows/release.yml:49`;
- test npm in `.github/workflows/release.yml:50`;
- test nativi updater e startup;
- packaging sui quattro target supportati;
- test installazione, disinstallazione e aggiornamento multi-istanza;
- pubblicazione npm con provenance in `.github/workflows/release.yml:181`.

Il repository contiene un solo workflow, `release.yml`. Non è presente un workflow `push` o `pull_request`. I gate sono quindi obbligatori durante la release, ma dal repository non risulta un gate CI automatico pre-merge. Le eventuali branch protection di GitHub non sono verificabili dal sorgente.

Le GitHub Action sono pinning a major version, non a commit SHA. Questo segue il contratto del repository, ma lascia un margine di rischio supply-chain superiore al pinning immutabile.

Gli archivi e `checksums.txt` vengono pubblicati dalla stessa release. Il checksum protegge da corruzione e mismatch, non da una compromissione della fonte di pubblicazione. npm usa provenance, mentre per gli archivi GitHub non risulta una firma Sigstore o equivalente.

`scripts/dev.sh` scarica un toolchain Go via HTTPS senza verifica esplicita del checksum quando Go non è disponibile. Questo è un rischio limitato al bootstrap dell'ambiente di sviluppo.

## Modificabilità dei gate

| Modifica | Difficoltà relativa | Punti principali |
| --- | --- | --- |
| Forzare review sempre o mai | Molto bassa | Configurazione live `harness.reviews` |
| Modificare il comportamento testuale del reviewer | Bassa | `.spynel/prompts/review.md`, istruzioni persistenti |
| Cambiare i requisiti della completion diretta | Media | `notification_message.go`, `manager.go`, test |
| Aggiungere una modalità di review | Media | `config.go`, `settings.go`, `review_mode.go`, `manager.go`, documentazione e test |
| Cambiare le transizioni task o goal | Medio-alta | `routes.go`, `manager.go`, `workflow.go`, template e test |
| Cambiare claim, lease o recovery | Alta | `manager.go`, `markdown.go`, lock platform-specifici e test concorrenti |
| Cambiare primary election o handoff | Alta | `instance/election.go`, server runtime, local API e test cross-processo |
| Rimuovere i gate in un fork privato | Bassa dal punto di vista legale e tecnico | Licenza MIT e logica concentrata |

### Punti che facilitano le modifiche

- La policy globale passa da `Harness.EffectiveTaskReviewRequired`.
- L'istruzione mostrata agli agenti è centralizzata in `review_mode.go`.
- La riconciliazione effettiva passa da `reconcileTaskTransition` e `reconcileGoalTransition`.
- I test di policy sono raccolti soprattutto in `review_policy_test.go` e `workflow_test.go`.
- Le route sono dichiarate in una singola funzione.

### Punti che aumentano il rischio delle modifiche

- `internal/orchestrator/manager.go` concentra claim, lease, dispatch, transizioni, recovery, hook e notifiche.
- Gli invarianti sono distribuiti tra file Markdown, lease JSON, stato in memoria, prompt e adapter provider.
- L'ordine delle operazioni in `scanOnce` è parte del contratto di correttezza.
- Una modifica alle transizioni deve restare coerente con template, documentazione, retention, notifiche e recovery.
- I prompt e le policy deterministiche possono sembrare equivalenti nella documentazione, pur avendo forza diversa in esecuzione.

## Priorità raccomandate

### Interventi immediati

1. Eliminare l'uso del PATH dell'utente nel processo sudo di disinstallazione. Usare helper con path assoluti verificati o un PATH trusted.
2. Introdurre una prova di consenso per le estensioni. Un marker scritto da `extension install`, un registro privato o una quarantena al primo discovery separerebbero codice installato da codice fornito dal repository.
3. Richiedere conferma alla prima adozione di un custom ACP proveniente da una configurazione workspace non ancora fidata.

### Interventi successivi

1. Rendere la policy di review immutabile per la durata del claim, salvandone uno snapshot nella lease o in uno stato non modificabile dall'implementatore.
2. Aggiungere una scansione limitata dei nuovi documenti terminali, così un file in `done` senza la lease attesa non produce effetti terminali senza validazione.
3. Applicare alla goal review un controllo d'indipendenza equivalente a quello dei task, quando il provider espone un'identità di thread affidabile.
4. Definire una transizione esplicita per le lease `hook_cancelled`, per esempio ritorno in coda o attesa amministrativa leggibile.
5. Rendere i numeric Telegram user ID la scelta predefinita e segnalare esplicitamente il rischio degli username.

### Hardening di sviluppo e release

1. Aggiungere un workflow CI per pull request e push che esegua almeno test, vet, build, DOX e smoke appropriato.
2. Valutare pinning immutabile delle GitHub Action.
3. Firmare gli archivi e i checksum con provenance verificabile indipendente dalla release asset source.
4. Verificare il checksum del toolchain scaricato da `scripts/dev.sh`.
5. Valutare una separazione interna di `manager.go` per rendere transizioni e recovery verificabili in moduli più piccoli.

## Verifiche dinamiche ancora necessarie

Prima di classificare i candidati come vulnerabilità confermate, servono prove isolate e ripetibili:

- repository temporaneo con estensione preinserita e nessuna precedente installazione;
- repository temporaneo con configurazione custom ACP e comando innocuo che scrive un marker;
- VM usa e getta per la disinstallazione sudo con un helper `npm` controllato nel PATH;
- test Telegram con cambio di proprietario dello username oppure fixture equivalente a livello adapter;
- test orchestrator che elimina la lease e sposta un task in `done` sotto `harness.reviews: always`;
- test goal review che usa lo stesso thread provider per planning e review;
- test che definisca il comportamento desiderato dopo `hook_cancelled`.

Le prove devono evitare credenziali reali, sistemi di produzione e side effect esterni.

## File e aree esaminate

### Architettura e contratti

- `AGENTS.md`
- `README.md`
- `docs/architecture.md`
- `docs/configuration.md`
- `docs/tasks-and-goals.md`
- `docs/extensions.md`
- `docs/provider-canary-threat-model.md`
- `internal/AGENTS.md`
- AGENTS locali di app, config, harness, orchestrator, local API, instance, extensions, channels e updater

### Runtime e sicurezza

- `internal/app/service.go`
- `internal/app/message_admission.go`
- `internal/config/config.go`
- `internal/config/settings.go`
- `internal/instance/election.go`
- `internal/localapi/server.go`
- `internal/localapi/client.go`
- `internal/localapi/socket_unix.go`
- `internal/channel/telegram/telegram.go`
- `internal/channel/telegram/identity.go`
- `internal/channel/whatsapp/whatsapp.go`
- `internal/extensions/hooks.go`
- `internal/extensions/install.go`
- `internal/harness/catalog.go`
- `internal/harness/acp.go`
- `internal/harness/supervisor.go`
- `internal/cli/uninstall.go`
- `internal/updater/install.go`
- `internal/updater/uninstall.go`

### Orchestrator e qualità

- `internal/orchestrator/manager.go`
- `internal/orchestrator/markdown.go`
- `internal/orchestrator/routes.go`
- `internal/orchestrator/review_mode.go`
- `internal/orchestrator/workflow.go`
- `internal/orchestrator/notification_message.go`
- `internal/orchestrator/review_policy_test.go`
- `internal/orchestrator/workflow_test.go`
- `internal/orchestrator/manager_test.go`
- template sotto `internal/workspace/templates/`

### Build e release

- `scripts/dev.sh`
- `scripts/smoke.sh`
- `scripts/package-native.sh`
- `install.sh`
- `uninstall.sh`
- `npm/install.js`
- `npm/update.js`
- `npm/test.js`
- `.github/workflows/release.yml`

## Conclusione

Spynel ha un control plane locale ben difeso e un sistema di orchestrazione durevole con gate reali, testati e relativamente facili da modificare. La garanzia cambia quando il provider o il repository diventano ostili. In quel modello, stato e policy risiedono nello stesso workspace che il harness può modificare, mentre estensioni e custom ACP possono trasformare la semplice adozione di un workspace in esecuzione di codice locale.

La priorità non è aggiungere più istruzioni ai prompt. Serve rendere esplicito il consenso per il codice del workspace, correggere il confine sudo e separare meglio lo stato di controllo dalle capacità di scrittura del harness.
