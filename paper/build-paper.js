const fs = require('fs');
const path = require('path');
const d = require('docx');
const {
  Document, Packer, Paragraph, TextRun, AlignmentType, SectionType,
  Table, TableRow, TableCell, WidthType, ShadingType, BorderStyle,
  convertInchesToTwip, ImageRun,
} = d;

const T = 'Times New Roman';
const SHOTS = path.join(__dirname, 'figures') + path.sep;

function P(text, o = {}) {
  const opts = Object.assign({ size: 20, after: 0, before: 0, indent: 0.2,
    align: AlignmentType.JUSTIFIED }, o);
  const runs = Array.isArray(text) ? text : [text];
  return new Paragraph({
    alignment: opts.align,
    indent: opts.indent ? { firstLine: convertInchesToTwip(opts.indent) } : undefined,
    spacing: { after: opts.after, before: opts.before, line: 230 },
    children: runs.map(r => typeof r === 'string'
      ? new TextRun({ text: r, size: opts.size, font: T })
      : new TextRun(Object.assign({ size: opts.size, font: T }, r))),
  });
}

const ROMAN = ['I','II','III','IV','V','VI','VII','VIII','IX','X'];
let secN = 0, subN = 0;

function SEC(text) {
  secN += 1; subN = 0;
  return new Paragraph({
    alignment: AlignmentType.CENTER,
    spacing: { before: 240, after: 110 },
    children: [new TextRun({ text: `${ROMAN[secN - 1]}.  ${text.toUpperCase()}`,
      size: 20, font: T })],
  });
}
function SUB(text) {
  subN += 1;
  return new Paragraph({
    alignment: AlignmentType.LEFT,
    spacing: { before: 150, after: 70 },
    children: [new TextRun({ text: `${String.fromCharCode(64 + subN)}.  ${text}`,
      size: 20, font: T, italics: true })],
  });
}

function cell(text, { w, bold = false, shade = null, size = 15, align = AlignmentType.LEFT } = {}) {
  const runs = Array.isArray(text) ? text : [text];
  return new TableCell({
    width: { size: w, type: WidthType.DXA },
    shading: shade ? { type: ShadingType.CLEAR, fill: shade } : undefined,
    margins: { top: 35, bottom: 35, left: 60, right: 60 },
    children: [new Paragraph({
      alignment: align, spacing: { after: 0, line: 215 },
      children: runs.map(r => typeof r === 'string'
        ? new TextRun({ text: r, size, bold, font: T })
        : new TextRun(Object.assign({ size, font: T }, r))),
    })],
  });
}
function table(widths, rows, opts = {}) {
  const total = widths.reduce((a, b) => a + b, 0);
  return new Table({
    columnWidths: widths,
    width: { size: total, type: WidthType.DXA },
    borders: {
      top: { style: BorderStyle.SINGLE, size: 4, color: '000000' },
      bottom: { style: BorderStyle.SINGLE, size: 4, color: '000000' },
      left: { style: BorderStyle.NONE }, right: { style: BorderStyle.NONE },
      insideHorizontal: { style: BorderStyle.SINGLE, size: 1, color: '999999' },
      insideVertical: { style: BorderStyle.NONE },
    },
    rows: rows.map((r, i) => new TableRow({
      tableHeader: i === 0,
      children: r.map((c, j) => cell(c, {
        w: widths[j], bold: i === 0, shade: i === 0 ? 'EFEFEF' : null,
        size: opts.size || 15,
        align: (opts.rightCols || []).includes(j) ? AlignmentType.RIGHT : AlignmentType.LEFT,
      })),
    })),
  });
}
const CAP = (n, t) => new Paragraph({
  alignment: AlignmentType.CENTER, spacing: { before: 80, after: 140 },
  children: [new TextRun({ text: `TABLE ${n}.  ${t.toUpperCase()}`, size: 16, font: T })],
});
const gap = (n = 120) => new Paragraph({ spacing: { after: n }, children: [] });

function FIG(file, wIn) {
  const png = fs.readFileSync(file);
  // read the IHDR for intrinsic size so the aspect ratio is never guessed
  const pw = png.readUInt32BE(16), ph = png.readUInt32BE(20);
  const hIn = wIn * ph / pw;
  return new Paragraph({
    alignment: AlignmentType.CENTER,
    spacing: { before: 90, after: 50 },
    children: [new ImageRun({
      type: 'png', data: png,
      transformation: { width: Math.round(wIn * 96), height: Math.round(hIn * 96) },
    })],
  });
}
function FIGCAP(n, t) {
  return new Paragraph({
    alignment: AlignmentType.LEFT,
    spacing: { after: 130 },
    children: [new TextRun({ text: `Fig. ${n}.  ${t}`, size: 16, font: T })],
  });
}

// ------------------------------------------------------------- title block
const head = [
  new Paragraph({
    alignment: AlignmentType.CENTER, spacing: { after: 160 },
    children: [new TextRun({
      text: 'Predicting and Recovering from Distributed Deadlocks under Partial Observation',
      size: 48, font: T })],
  }),
  new Paragraph({
    alignment: AlignmentType.CENTER, spacing: { after: 60 },
    children: [new TextRun({ text: 'Fida Shameer, Avanti Golatkar', size: 22, font: T })],
  }),
  new Paragraph({
    alignment: AlignmentType.CENTER, spacing: { after: 40 },
    children: [new TextRun({ text: 'School of Computer Science and Engineering',
      size: 20, font: T, italics: true })],
  }),
  new Paragraph({
    alignment: AlignmentType.CENTER, spacing: { after: 40 },
    children: [new TextRun({ text: 'Vellore Institute of Technology, Vellore, India',
      size: 20, font: T, italics: true })],
  }),
  new Paragraph({
    alignment: AlignmentType.CENTER, spacing: { after: 260 },
    children: [new TextRun({ text: '24BAI0180  ·  24BKT0119', size: 20, font: T })],
  }),
];

// ------------------------------------------------------------- body part A
const A = []; const a = (...xs) => xs.forEach(x => A.push(x));

a(P([
  { text: 'Abstract—', bold: true, italics: true },
  'Detecting a distributed deadlock is a solved problem. Deciding which service to kill is not. The usual rule picks whichever candidate is cheapest to roll back, which is correct inside a single database but ignores the services left queued behind the one you kill. We describe a framework that does two things. It predicts deadlocks by learning lock acquisition orders from the same event stream the detector already reads, and it chooses victims using a criticality-weighted blast radius. Both are tested against three baselines across ten experiments with twenty seeded trials each. Three results stand out. Blast radius on its own is harmful. It starves the cheapest victims, throughput falls to one or two transactions at sixty services, and it does this while posting the best score on the metric it optimises. Paired with an anti-starvation term it completes roughly three times as many transactions as the prior art. Lock-order prediction reaches precision 1.000 under message delay and loss, but its lead time turns negative under heavy contention, which is where deadlocks cost the most. Message loss also produces a failure that single-process tools never encounter: the predictor accuses a service that is behaving correctly, and precision does not catch it. We add an attribution metric that does. We also report a ground-truth leak found in our own evaluation harness, whose correction reversed one of our results.',
  ], { size: 18, indent: 0, after: 120 }));

a(P([
  { text: 'Index Terms—', bold: true, italics: true },
  'distributed systems, deadlock detection, deadlock recovery, lock ordering, victim selection, fault tolerance, empirical evaluation',
  ], { size: 18, indent: 0, after: 160 }));

// I. INTRODUCTION
a(SEC('Introduction'));
a(P('A deadlock happens when two or more services each hold something the others need. Nobody can move, and the situation does not clear on its own. One participant has to be terminated and its work discarded.', { indent: 0 }));
a(P('Two questions follow from that. How do you detect the cycle, and who do you kill. The first has been settled since the 1980s. The second has had much less attention, and the answers that exist were written for a single database.'));
a(P('A distributed setting differs in three ways that matter here. There is no shared clock, so events cannot be ordered by timestamp. Observations arrive late, out of order, or not at all. And terminating one service can strand a queue of others behind it, which cannot happen inside a single database because there are no other services to strand.'));
a(P('That third difference is where this work starts. Conventional victim selection asks which candidate is cheapest to roll back and looks at each one in isolation. In a connected system we think that is the wrong question, because the cheapest candidate may be the one the largest number of other services are waiting on. We score candidates instead by a criticality-weighted blast radius, meaning the victim plus everything transitively queued behind it from outside the cycle.'));
a(P('A deadlock is also often predictable before anyone blocks. If one service consistently takes lock A then lock B, and another takes B then A, the two will deadlock as soon as their execution overlaps. That is knowable from acquisition history alone. Tools inside a single process already use this. Carrying it across a network, where observations are unreliable, is not a straightforward port.'));
a(P('The paper makes four contributions.'));
a(P([{ text: '1) ', bold: true }, 'Lock-order prediction where observation is partial, along with a failure mode specific to that setting. A lost observation makes the predictor accuse a service that has done nothing wrong. We show precision does not detect this and introduce an attribution metric that does.']));
a(P([{ text: '2) ', bold: true }, 'A measured demonstration that blast-radius victim selection is destructive without an anti-starvation partner term, and effective with one.']));
a(P([{ text: '3) ', bold: true }, 'An account of why self-calibrating the score weights is mis-specified for this problem rather than simply tuned badly.']));
a(P([{ text: '4) ', bold: true }, 'A ground-truth leak found in our own evaluation harness. We report it because it is a general hazard, it fails silently, and correcting it reversed one of our results.']));

// II. RELATED WORK
a(SEC('Related Work'));
a(SUB('Distributed deadlock detection'));
a(P('Detection using wait-for graphs and probe messages is long established. Chandy, Misra and Haas [1] give the canonical algorithm and Elmagarmid [2] surveys the design space. We apply this work rather than extend it. Our dependency monitor builds a wait-for graph and finds cycles with Tarjan strongly-connected components.', { indent: 0 }));
a(P('Rowicki et al. [3] are closer to our setting in what the detector is allowed to see. Their monitors are black boxes that watch only the messages crossing service boundaries, and the algorithm is proved correct in Coq. The difference is in what the detector is for. They establish that a deadlock has occurred; our monitor also has to decide what to do about it.'));
a(SUB('Lock services'));
a(P('Gray and Cheriton [4] introduced leases as a fault-tolerant consistency mechanism. Chubby [5] is the reference consensus-backed lock service. ZooKeeper [6] follows it with a wait-free interface in which clients build locks from data nodes and watches rather than blocking on the server, and etcd descends from the same line. Kleppmann [7] shows a lease on its own is not enough when a client stalls, and that a monotonic fencing token is needed. We take that token from the etcd revision counter, which increases across the whole keyspace, so we do not have to maintain a counter of our own.', { indent: 0 }));
a(SUB('Victim selection'));
a(P('Production databases and the distributed scheme in US 9,104,989 B2 [8] pick the candidate cheapest to roll back. DSLR [9] decentralises lock management over RDMA to remove a coordinator bottleneck, which is about how locks are granted rather than how deadlocks are resolved.', { indent: 0 }));
a(P('HAWK [10] is the closest recent work. It builds a hierarchical detection topology from a predicted access graph, and it does change the victim rule. Where several detected cycles overlap, it aborts the transaction that appears in the most of them, breaking ties by transaction identifier. The effect is to minimise the number of transactions aborted. It does not weigh what any individual abort costs, and in particular it does not count the work queued behind a candidate. That quantity is what this paper scores on, so the two are complementary rather than competing: HAWK reduces how many victims are needed and we address which one to pick among them.'));
a(P('Tian et al. [11] come at contention from the scheduling side. They ask which waiting transaction should be granted a lock next, show that the usual first-come-first-served order is far from optimal, and find that the best heuristic favours the transaction blocking the largest number of others. The intuition, that a request matters in proportion to how many others are queued behind it, is the one we apply to the opposite decision. They state deadlock as out of scope and assume a separate module resolves it. This paper is about that module.'));
a(SUB('Lock-order analysis'));
a(P('lockdep [12] in the Linux kernel and Goodlock [13] for Java find inconsistent lock ordering inside a single process. The modern form of this idea is dynamic deadlock prediction, which asks whether an observed execution could have deadlocked under some other interleaving. Tun\u00e7 et al. [14] show that sound and complete prediction is intractable and give a linear-time sound algorithm for the class they call sync-preserving deadlocks. van den Heuvel et al. [15] sharpen the precision of the same analysis using partial orders over the trace.', { indent: 0 }));
a(P('Every one of these assumes a complete trace of lock operations in the correct order. That assumption is exactly what this paper gives up, and Section VI-D shows it is not a minor relaxation, because a single lost event turns a correct warning into a confident accusation against the wrong service.'));
a(P('Rakha et al. [16] approach anticipation from a different direction, training sequence models on database lock traces to forecast which lock a transaction will take next. That predicts the identity of the next lock. We predict whether an ordering between two locks is hazardous, which is a property of a pair rather than of a single acquisition.'));
a(P('That these faults still occur in production is not in doubt. Tu et al. [17] study 171 concurrency bugs in six large Go systems, among them Docker and Kubernetes, and report that more than half arise from the language\u2019s own concurrency constructs rather than from classical shared-memory mistakes.'));
a(SUB('Monitoring under incomplete observation'));
a(P('Our problem is an instance of a more general one: deciding a property of a system from an event stream that is missing some of its events. Kushwaha et al. [18] formalise this for finite-state properties. They model loss as a relation over event symbols and build a monitor that tracks the set of states the system could be in, reporting a violation only when every completion of the observed stream leads to one. Mahe et al. [19] treat the distributed case, where each component logs separately and some components are not observed at all.', { indent: 0 }));
a(P('The principle we arrived at is theirs. When information is missing, widen what you treat as possible rather than commit to one completion. Our gap-skipping rule discards the held-set instead of inferring across the gap, and our confirmation step withholds a warning until the evidence survives a second interval. Section VI-D is what happens without that discipline.'));

// III. SYSTEM DESIGN
a(SEC('System Design'));
a(P('Fig. 1 shows the end-to-end workflow. Services request locks from etcd and report their own lock events. The dependency monitor reads that single event stream and keeps two structurally different records from it. Detection feeds the recovery engine, while prediction produces an operator warning that needs no abort. The chosen victim is signalled across a process boundary, which releases its locks and returns the system to the top of the loop. Shaded blocks are contributed by this work and everything else is applied.', { indent: 0 }));
a(FIG(path.join(__dirname, 'figures', 'fig1-architecture.png'), 3.30));
a(FIGCAP(1, 'End-to-end workflow. One event stream feeds two records: a wait-for graph for detection and a persistent lock-order graph for prediction. Recording sits on the critical path and explanation generation does not.'));

a(SUB('Two graphs from one event stream'));
a(P('Services report three kinds of event, acquired, waiting and released. Each carries a per-service monotonic sequence number and no wall-clock timestamp, since we assume no shared clock. We keep two different records from that one stream.', { indent: 0 }));
a(P('The wait-for graph is reactive. An edge runs from a blocked service to the holder of the lock it wants, and a cycle in it is a deadlock. Edges disappear as locks are released.'));
a(P('The lock-order graph is historical and is never cleared. When a service holds one lock and takes another, that ordering is recorded permanently, because the behaviour stays hazardous long after both locks are free. This separation is what makes prediction possible at all. Evidence of a dangerous ordering has to outlive the locks it concerns.'));
a(P('The graph does not grow without bound. It grows with the number of distinct lock pairs, not with time or traffic. In the demonstration run of Fig. 2 it holds three edges after nine hundred lock events.'));

a(SUB('Lock protocol'));
a(P('Locks live in etcd. Acquisition grants a lease with a time to live, then issues a transaction conditioned on create_revision == 0, which means write only if the key is unheld. Competing transactions are serialised through Raft [20] so exactly one succeeds. Mutual exclusion comes from consensus and not from any structure we maintain. The revision assigned to the successful write is used as the fencing token. A holder renews while it works, and a holder that stops renewing has its key removed by etcd, so a crashed service has its lock reclaimed without anything in our framework detecting the crash.', { indent: 0 }));

a(SUB('Prediction under partial observation'));
a(P('Three mechanisms deal with unreliable observation. The first is per-service reorder buffering, which replays events in sequence order so a message that overtakes an earlier one cannot create an ordering that never happened. The second handles an event that never arrives at all. After one reporting interval the gap is skipped and the recorded held-set for that service is thrown away. Throwing it away is the important part. The lost event could have been either an acquisition or a release, so the held-set is no longer knowable, and any ordering inferred across the gap would be a guess presented as an observation. The third is confirmation. A candidate warning is re-checked at the end of the following interval and reported only if the evidence is still there, which costs one interval of lead time and buys accuracy.', { indent: 0 }));

a(SUB('Victim selection'));
a(P('Every candidate in a detected cycle gets a score and the lowest one is chosen. The score keeps a prediction of damage separate from preferences about which victims are acceptable.', { indent: 0 }));
a(P('Predicted damage combines criticality-weighted blast radius with rollback cost. Blast radius counts the victim and every service transitively queued behind it from outside the cycle. Cycle members are excluded by construction. Inside a deadlock every member is transitively blocked on every other, so including them would give all candidates the same score, and they are the services the abort releases anyway.'));
a(P('Two preference terms follow. Partition suspicion is applied as a tier and not as a weight. The indicator is binary while costs are unbounded, so no fixed weight expresses the intent reliably, and an unreachable service may never receive the instruction at all, which means the action fails rather than costing more. Criticality itself is an input to the score and not something we compute. An operator can supply it, or it can be estimated from runtime data: Yang et al. [21] infer how strongly one service depends on another from distributed tracing spans, which would serve. An anti-starvation term penalises a candidate in proportion to how often it has already been sacrificed. Section VI-B explains why that term turns out to be necessary.'));

// IV. IMPLEMENTATION AND DEMONSTRATION
a(SEC('Implementation and Demonstration'));
a(P('The framework is about 4,900 lines of Go using only the standard library. It talks to etcd through the v3 HTTP and JSON gateway. The repository carries 36 tests, five of which run against a live etcd instead of against our own data structures.', { indent: 0 }));
a(P('The demonstration harness runs each service as its own operating system process. It reads lock state directly from etcd every 50 milliseconds and compares that against what the services report, so the figures below are not a program checking itself. Figures 2 to 6 come from one session on a MacBook Air running Go 1.27.1 on darwin/arm64 against etcd 3.7.2.'));
a(P('Fig. 2 is a twenty second run with three services, two of which take locks L0 and L1 in opposite orders deliberately. The run produces one confirmed lock-order warning at 1.4 seconds, twelve deadlocks detected and broken by signalling a victim across a process boundary, and no exclusion failures. The victim alternates between the two services rather than settling on one, which is the anti-starvation term at work. Ten seconds in, the harness sends SIGKILL to the process holding L0. Nothing in the framework notices that crash. The lock comes back 2.2 seconds later because the lease expired, which is the behaviour described in Section III-B.'));
a(P('Fig. 3 and Fig. 4 show the same system with ten services and a live view of the state. In Fig. 3 the service named audit holds L2 and is waiting on L0, while billing holds L1 and is waiting on L2, which audit holds. That pair of rows is what a deadlock looks like from outside. The counters in Fig. 4 are from later in the same run, after 3,107 lock events and 149 broken deadlocks, still with no exclusion failures.'));
a(P('Fig. 5 is the part of the test suite that runs against a live etcd. Those five tests carry more weight than the rest, because they check exclusion and lease-based reclamation against a real consensus-backed store rather than against our own data structures. Fig. 6 is one experiment run live, and is discussed in Section VI-A.'));

// ------------------------------------------------------------- figure gallery
const G = []; const g = (...xs) => xs.forEach(x => G.push(x));
const M = []; const m = (...xs) => xs.forEach(x => M.push(x));
const H = []; const h = (...xs) => xs.forEach(x => H.push(x));

g(FIG(SHOTS + 'fig2-e2e-run.png', 6.50));
g(FIGCAP(2, 'A twenty second end-to-end run with three services as separate OS processes against etcd. One confirmed lock-order warning, twelve deadlocks broken by signal, no exclusion failures, and a lock reclaimed 2.2 s after its holder was killed with SIGKILL. The victim alternates between inventory and payment rather than settling on one.'));

g(FIG(SHOTS + 'fig3-dashboard-deadlock.png', 6.40));
g(FIGCAP(3, 'Live view of the same system with ten services. audit holds L2 and waits on L0; billing holds L1 and waits on L2, which audit holds. The right panel reads lock ownership straight from etcd. Exclusion is checked every 50 ms and the violation count is zero.'));

g(FIG(SHOTS + 'fig4-counters.png', 5.60));
g(FIGCAP(4, 'Counters from later in the same run. After 3,107 lock events and 149 deadlocks broken there are still no exclusion failures.'));

// V. METHODOLOGY
m(SEC('Evaluation Methodology'));
m(P('One principle governs the experiments. Every mechanism we claim is evaluated against a metric it does not itself optimise. Scoring a victim-selection policy on services disrupted, which is the quantity it minimises, is close to circular. The metric that carries the weight is completed transactions, which no policy optimises directly and which cannot be improved by a policy that produces attractive disruption figures by bringing the system to a halt.', { indent: 0 }));
m(P('There are three baselines and each uses a different class of information. The timeout baseline kills the longest waiter and uses no graph information at all. The cost-only baseline kills the candidate cheapest to roll back, which is the prior art [8] and is deliberately strong, because on a single machine it is the right answer. The youngest-transaction baseline kills the most recently blocked candidate, which is the classical transaction-manager rule and the one a practitioner would reach for first.'));
m(P('Two harnesses are used because they serve purposes that cannot be combined. Policy comparison runs in a deterministic seeded discrete-event simulation, because a fair comparison needs every policy to meet an identical workload, and with real processes a difference between policies could not be separated from timing variance. The second harness is the one behind Figs. 2 to 4. It runs services as separate processes against a real etcd with crash injection by SIGKILL, and it is deliberately non-deterministic. The first establishes how the policy behaves and the second establishes that the system does.'));
m(P('All figures are twenty trials with 95% confidence intervals. A difference is reported as real only where the intervals do not overlap. Runs are seeded and reproduce exactly across architectures and compiler versions, which we checked by running the full suite on Linux under Go 1.24.7 and on macOS under Go 1.27.1 and comparing the output. The only quantities that differ between machines are the two wall-clock measurements in H3 and H6.'));

// VI. RESULTS

m(SEC('Results'));
m(CAP('I', 'Summary of experimental outcomes'));
m(table([430, 1480, 2290], [
  ['', 'Hypothesis', 'Outcome'],
  ['H1', 'Disruption vs cost-only', '3.5% worse, separated at 95%'],
  ['H2', 'Recovery time vs timeout', '65 to 84% faster, all levels'],
  ['H3', 'Framework overhead', '0% throughput, +49% CPU'],
  ['H4', 'Crash and stale-lock recovery', '0 double-grants, 20/20 progressed'],
  ['H5', 'Prediction quality', 'Precision 1.000, recall 1.00 to 0.725'],
  ['H6', 'Explanation recording cost', '+1.02%, inside trial noise'],
  ['H7', 'Pillar 2 ablation', 'Fairness gives about 3x throughput'],
  ['H8', 'Partition handling', '22.9 futile episodes to 0'],
  ['H9', 'Self-calibration', 'Loses on its own training target'],
  ['H10', 'Deadlock frequency', 'Best at every contention level'],
]));
m(gap(140));

m(SUB('Deadlock frequency against the baselines'));
m(P('Counting deadlocks on its own is misleading, because a policy that leaves deadlocks standing records fewer of them. The services involved never get back to work, so they never collide again. We therefore report deadlocks per thousand completed transactions, which is how much deadlock the system suffers per unit of work it actually finishes. Table II gives the result and Fig. 6 shows the same experiment run live.', { indent: 0 }));
m(CAP('II', 'Deadlocks per thousand completed transactions'));
m(table([1050, 740, 760, 740, 870], [
  ['Policy', 'Low', 'Medium', 'High', 'Completed (high)'],
  ['timeout', '192.3', '1212.0', '2067.9', '108'],
  ['cost-only', '186.6', '3713.9', '655410.3', '1'],
  ['youngest', '190.5', '4200.5', '687888.9', '1'],
  ['adaptive (ours)', '178.0', '1585.0', '1406.6', '332'],
], { rightCols: [1, 2, 3, 4] }));
m(gap(140));
m(P('The six-figure values are not a result. Cost-only and youngest complete a single transaction per run at high contention, so the ratio divides by almost nothing. The finding is the collapse itself. Both of them keep choosing the same victim, which therefore never finishes anything. That two baselines consuming entirely different information fail in the same way strengthens the point, because it rules out the objection that we picked a weak opponent.'));
m(P('Two qualifications belong here. At low contention our advantage sits inside the margin of error and we claim no win. At medium contention the timeout baseline posts a better ratio than we do while completing about 40% fewer transactions, because throttling a system is a way of avoiding deadlock that nobody would choose. Both columns have to be read together.'));

h(FIG(SHOTS + 'fig5-etcd-tests.png', 5.10));
h(FIGCAP(5, 'The five tests that run against a live etcd, out of thirty-six in the suite. These are the evidence behind the safety claim in Section VI-E, since exclusion here is enforced by Raft rather than by a structure we wrote. The fencing-token test prints the two etcd revisions it observed and checks that the second is larger.'));

h(FIG(SHOTS + 'fig6-h10.png', 5.80));
h(FIGCAP(6, 'Deadlock frequency against the three baselines, run live at five trials. At high contention cost-only and youngest complete one transaction each, so the ratio in the final column divides by almost nothing. The finding is the collapse in the completed column, not the size of the ratio. The tables in Section VI cite the twenty-trial run.'));

// ------------------------------------------------------------- body part B
const B = []; const b = (...xs) => xs.forEach(x => B.push(x));

b(SUB('Blast radius alone is destructive'));
b(P('Table III turns each part of the victim-selection score on and off. The column headed disrupted is the quantity the policy minimises. The column headed completed is independent of it.', { indent: 0 }));
b(CAP('III', 'Ablation of the victim-selection score'));
b(table([1350, 710, 680, 710, 710], [
  ['Configuration', '30 cmp', '30 dis', '60 cmp', '60 dis'],
  ['cost-only (prior art)', '113', '6.88', '111', '11.98'],
  ['blast radius only', '2', '4.71', '1', '9.76'],
  ['blast + self-calibration', '2', '4.71', '1', '9.76'],
  ['blast + fairness', '348', '6.70', '346', '11.77'],
  ['full (all three)', '332', '6.76', '335', '11.86'],
], { rightCols: [1, 2, 3, 4] }));
b(gap(140));
b(P('Minimising blast radius with no anti-starvation partner completes one or two transactions at 30 and 60 services, against 111 to 113 for the prior art, and it does so while reporting the best disruption figure anywhere in the suite. It keeps picking the same low-cost victims and those victims never complete. This is the clearest illustration we have of why a policy must not be judged on the metric it optimises. On its own scorecard the mechanism looks excellent while the system is doing nothing.'));
b(P('Adding the anti-starvation term is what makes the approach beat the prior art on an independent metric, at roughly three times the completed count at 30 and 60 services and at comparable disruption.'));

b(SUB('Prediction is accurate but mistimed'));
b(P('The predictor never raised a false warning in any condition we tested. Precision and attribution are both 1.000 throughout Table IV, while recall falls from 1.000 to 0.725 as delay and loss increase. Missing a warning is acceptable to us and raising a wrong one is not.', { indent: 0 }));
b(CAP('IV', 'Prediction under delayed and lost observations'));
b(table([1200, 700, 780, 850, 620], [
  ['Condition', 'Prec.', 'Attrib.', 'Recall', 'Lead'],
  ['clean', '1.000', '1.000', '1.000', '+21.4'],
  ['delay 3', '1.000', '1.000', '0.988', '+2.4'],
  ['delay 3, loss 5%', '1.000', '1.000', '0.887', '+12.8'],
  ['delay 8, loss 15%', '1.000', '1.000', '0.725', '+4.5'],
], { rightCols: [1, 2, 3, 4] }));
b(gap(140));
b(P('Lead time is a different story. It inverts with contention, as Table V shows.'));
b(CAP('V', 'Prediction lead time against contention'));
b(table([1150, 1200, 1300], [
  ['Contention', 'Lead (ticks)', 'Outcome'],
  ['Heavy', '-474.1 ± 242.2', 'Deadlock first'],
  ['Moderate', '+14.5 ± 8.1', 'Marginal warning'],
  ['Light', '+669.0 ± 161.3', 'Useful warning'],
  ['Very light', '+2144.5 ± 406.7', 'Large warning'],
], { rightCols: [1] }));
b(gap(140));
b(P('The cause is structural rather than a tuning problem. The check needs a service to successfully complete an out-of-order acquisition before any evidence exists. Under heavy contention that service deadlocks on its first attempt, so the deadlock arrives before the warning it was supposed to produce. The mechanism pays off only where contention is light enough for conflicting orderings to be exercised separately before they overlap, which is also where deadlocks cost the least. No amount of optimisation can generate evidence that was never produced.'));

b(SUB('A failure mode specific to partial observation'));
b(P('Message loss creates a failure that single-process tools never meet. Take a service that acquires lock A, releases it, then acquires lock B. It never holds both. If the release is lost, the monitor still believes A is held, it then sees B acquired, and it records an ordering A to B that never happened. A fabricated ordering can be the exact reverse of a genuine one held by another service, so the result is a confident warning naming a service that has done nothing wrong. Under 15% loss an earlier version of our gap handling emitted warnings at tick 40 for a conflict that could not physically arise before tick 400.', { indent: 0 }));
b(P([ 'Precision does not detect this. The fabricated warnings named a genuinely hazardous lock pair, for the wrong reason and naming the wrong services, so precision stayed at 1.000 the whole time. We therefore report an ', { text: 'attribution', italics: true }, ' metric as well: whether the two services a warning names do in fact hold opposing orderings. That metric is what exposed the fault, and we think it should accompany precision in any evaluation of lock-order analysis where observation is unreliable.' ]));

b(SUB('Safety, overhead and recovery'));
b(P('Exclusion held in every configuration. Across twenty simulated trials with crash injection there were no double grants and all twenty runs kept making progress after the crash. The load-bearing evidence is not that count but the etcd integration tests in Fig. 5, where exclusion is enforced by Raft rather than by anything we wrote. One of those tests performs fifteen acquisitions across eight services and checks for violations directly.', { indent: 0 }));
b(P('Overhead is small where it matters. The framework completes exactly the same number of transactions whether it is switched on or off, at 3,689 in both cases, which is inside the bound of 10% we stated before measuring. CPU cost is higher, at +49%, though in absolute terms that is 47 ms against 31 ms per run. Recording the data that explanations are generated from costs +1.02%, which is inside trial noise. Explanation generation itself runs offline and is not on the critical path.'));
b(P('Against the timeout baseline, recovery is 65 to 84% faster at every contention level tested, with the intervals disjoint in all three cases.'));

b(SUB('Self-calibration is mis-specified'));
b(P('We implemented least-mean-squares calibration of the score weights. Establishing that it does not work took three corrections, each of which was hiding the next. The first was an update rule using the sign of each feature, which moved all weights in lockstep. The second was a regression target that included the unbounded fairness term, which drove both cost weights to zero. The third was a feedback signal computed by the harness as blast radius, which is one of the inputs the policy already has, so the learner could only rediscover its own definition.', { indent: 0 }));
b(P('With all three fixed the learner behaves properly, and that is exactly why it fails. Table VI gives the result at the highest criticality skew we tested, judged on work lost, which is the quantity it is trained on and therefore the column it is entitled to be judged on.'));
b(CAP('VI', 'Self-calibration at 10x criticality skew'));
b(table([1000, 900, 900, 950], [
  ['Learning rate', 'Completed', 'Disrupted', 'Work lost'],
  ['0.00 (off)', '563', '11.36', '1.217'],
  ['0.08', '514', '17.42', '1.249'],
  ['0.30', '512', '17.13', '1.246'],
], { rightCols: [1, 2, 3] }));
b(gap(140));
b(P('It loses on all three columns including its own target. The reason is specific and we can state it. The only quantity observable after an abort is the work destroyed, and that does not depend on blast radius, so a correct regression drives the blast-radius weight to zero. But blast radius is what governs throughput. Freezing that weight at zero with no learning at all reproduces almost the whole loss, which places the failure in where a correct learner has to converge rather than in how it converges. Put generally, a victim-selection score cannot be calibrated by regression onto a single observed cost component when it has to balance costs that component does not contain. The framework ships with calibration switched off.'));

// VII. PARTITION
b(SUB('Partition handling'));
b(P('One service is cut off at tick 1000 for 1500 ticks. It is still running and still holding its locks, but it is unreachable and silent. With no partition handling the engine starts aiming at that service 22.9 times per run on average and the abort instruction never arrives. With handling enabled that figure is zero and completed transactions rise from 876 to 930, with the intervals disjoint.', { indent: 0 }));
b(P('We cannot separate our two partition mechanisms with this experiment and we say so. The weighted term and the tier preference both drive futility to zero here. A separate measurement shows why. The cost gap between a reachable and an unreachable candidate is near zero in this workload, so a fixed weight of 2.0 wins by magnitude rather than by design. A workload with larger cost spreads would overrule it. The tier does not depend on any margin, which is why both ship.'));

// VIII. THREATS
b(SEC('Threats to Validity'));
b(SUB('A ground-truth leak in our own harness'));
b(P('Our simulator was initially handing the policy ground truth. Rollback cost was read from true service state rather than from delivered events, so the estimate the policy reasoned about was exactly the work its abort went on to destroy. It could not be wrong, and every policy in the comparison was being evaluated on decisions made with information no deployment has.', { indent: 0 }));
b(P('The defect stayed invisible until we pushed self-calibration hard enough to expose it. The snapshot is now built from an observed held-count maintained only from the event stream, and all ten experiments were re-run. One result reversed. On services disrupted the adaptive policy is now 3.5% worse than the baseline, because it deliberately trades disruption for throughput. We report that unchanged. The general hazard, an evaluation harness that flatters the system it measures and fails silently while doing it, applies well beyond this work.'));
b(SUB('A second measurement defect in the demonstration harness'));
b(P('During end-to-end testing we found a related fault. The harness counted a lock as suspiciously held whenever its holder was absent from the live service map, and the crash-injection path removes the killed service from that map so it is never signalled again. A process that held more than one lock therefore produced spurious counts for the gap between its two leases expiring. The condition needed a slow enough machine for those two expiries to separate by more than one polling interval, which is why it appeared on one laptop and not on another. The check now tests the holder against the set of services the run started, which does not change when a service is killed. No lock was ever granted to two services, and the underlying safety property was never affected, but the counter was reporting a failure that had not happened.', { indent: 0 }));
b(P('Both defects are the same shape. A measurement reads state that the thing being measured has already changed. We record it because this is now the second instance in one project and the pattern seems worth naming.'));
b(SUB('Remaining limitations'));
b(P('The comparison sweeps run against the simulator rather than against etcd end to end, which matters more than usual given the leak described above. Porting H1, H7 and H10 to the process-based harness would strengthen them, at the cost of much wider variance.', { indent: 0 }));
b(P('The etcd deployment is a single node, so partition behaviour is injected into the simulator rather than produced by cutting a real network. A three-node cluster with traffic control applied to one member would make H8 a direct measurement.'));
b(P('H8 also cannot separate the weighted partition term from the tier preference, for the reason given in Section VI-G.'));
b(P('We model the lock layer and not the data layer. Services coordinate through shared locks but do not read or write business records, so rollback cost is a count of locks held rather than a measure of real work discarded.'));


// IX. CONCLUSION
b(SEC('Conclusion and Future Work'));
b(P('Of the three mechanisms we proposed, one holds in corrected form, one holds only where it is least needed, and one does not hold at all. We report all three. The claim we are prepared to defend is that blast-radius-aware victim selection is viable only when it is paired with an anti-starvation term, and that in that combination it beats both cost-based and time-based selection on throughput and on deadlocks per unit of completed work.', { indent: 0 }));
b(P('The extension we think is most promising goes at the prediction timing limitation directly. Instead of learning orderings by watching successful acquisitions, which is evidence heavy contention prevents from appearing, orderings could be pulled out of service source code by static analysis. The integration point already exists, because the lock-order graph serialises to and from a file, so an analyser emitting the same format would be loaded at startup and the existing warning pipeline would carry on unchanged. The known difficulty is lock identity. Source-level analysis resolves lock classes rather than instances and therefore over-approximates, which is the same limitation lockdep carries for per-object locks. That points to a hybrid, with static analysis seeding the graph and runtime observation confirming, and it maps onto the candidate-then-confirmed structure we have already built.'));

// REFERENCES
b(new Paragraph({
  alignment: AlignmentType.CENTER, spacing: { before: 240, after: 110 },
  children: [new TextRun({ text: 'REFERENCES', size: 20, font: T })],
}));
const refs = [
  'K. M. Chandy, J. Misra, and L. M. Haas, \u201cDistributed deadlock detection,\u201d ACM Trans. Comput. Syst., vol. 1, no. 2, pp. 144\u2013156, 1983.',
  'A. K. Elmagarmid, \u201cA survey of distributed deadlock detection algorithms,\u201d ACM SIGMOD Rec., vol. 15, no. 3, pp. 37\u201345, 1986.',
  'R. J. Rowicki, A. Francalanza, and A. Scalas, \u201cCorrect black-box monitors for distributed deadlock detection: formalisation and implementation,\u201d 2025, doi: 10.1145/3763069.',
  'C. Gray and D. Cheriton, \u201cLeases: an efficient fault-tolerant mechanism for distributed file cache consistency,\u201d in Proc. SOSP, 1989, pp. 202\u2013210.',
  'M. Burrows, \u201cThe Chubby lock service for loosely-coupled distributed systems,\u201d in Proc. OSDI, 2006, pp. 335\u2013350.',
  'P. Hunt, M. Konar, F. P. Junqueira, and B. Reed, \u201cZooKeeper: wait-free coordination for Internet-scale systems,\u201d in Proc. USENIX ATC, 2010.',
  'M. Kleppmann, \u201cHow to do distributed locking,\u201d 2016.',
  '\u201cPriority based victim selection for distributed deadlock resolution,\u201d U.S. Patent 9,104,989 B2.',
  'D. Y. Yoon, M. Chowdhury, and B. Mozafari, \u201cDistributed lock management with RDMA: decentralization without starvation,\u201d in Proc. SIGMOD, 2018, pp. 1571\u20131586.',
  'R. Zhang, Z. Ye, J.-P. Zhu, P. Cai, X. Zhou, D. Cai, and L. Qian, \u201cHAWK: a workload-driven hierarchical deadlock detection approach in distributed database system,\u201d Proc. VLDB Endow., vol. 18, no. 10, pp. 3682\u20133694, 2025.',
  'B. Tian, J. Huang, B. Mozafari, and G. Schoenebeck, \u201cContention-aware lock scheduling for transactional databases,\u201d Proc. VLDB Endow., vol. 11, no. 5, pp. 648\u2013662, 2018.',
  'Linux kernel documentation, \u201cRuntime locking correctness validator (lockdep).\u201d',
  'K. Havelund, \u201cUsing runtime analysis to guide model checking of Java programs,\u201d in Proc. SPIN, 2000, pp. 245\u2013264.',
  'H. C. Tun\u00e7, U. Mathur, A. Pavlogiannis, and M. Viswanathan, \u201cSound dynamic deadlock prediction in linear time,\u201d Proc. ACM Program. Lang., vol. 7, no. PLDI, art. 177, 2023.',
  'B. van den Heuvel, M. Sulzmann, and P. Thiemann, \u201cPartial orders for precise and efficient dynamic deadlock prediction,\u201d arXiv:2502.20070.',
  'M. S. Rakha, A. Sorrenti, G. Stager, W. Rjaibi, and A. Miranskyy, \u201cLock prediction for zero-downtime database encryption,\u201d arXiv:2506.23985, 2025.',
  'T. Tu, X. Liu, L. Song, and Y. Zhang, \u201cUnderstanding real-world concurrency bugs in Go,\u201d in Proc. ASPLOS, 2019, pp. 865\u2013878.',
  'P. Kushwaha, R. Purandare, and M. B. Dwyer, \u201cOptimal runtime verification of finite state properties over lossy event streams,\u201d arXiv:2004.04214, 2020.',
  'E. Mahe, B. Bannour, C. Gaston, A. Lapitre, and P. Le Gall, \u201cDealing with observability in interaction-based offline runtime verification of distributed systems,\u201d arXiv:2212.09324, 2022.',
  'D. Ongaro and J. Ousterhout, \u201cIn search of an understandable consensus algorithm,\u201d in Proc. USENIX ATC, 2014, pp. 305\u2013319.',
  'T. Yang, J. Shen, Y. Su, X. Ling, Y. Yang, and M. R. Lyu, \u201cAID: efficient prediction of aggregated intensity of dependency in large-scale cloud systems,\u201d arXiv:2109.04893, 2021.',
];
refs.forEach((r, i) => b(new Paragraph({
  alignment: AlignmentType.JUSTIFIED,
  indent: { left: convertInchesToTwip(0.22), hanging: convertInchesToTwip(0.22) },
  spacing: { after: 40, line: 210 },
  children: [new TextRun({ text: `[${i + 1}]\t${r}`, size: 16, font: T })],
})));

// ------------------------------------------------------------------- assemble
const PAGE = { size: { width: 12240, height: 15840 },
  margin: { top: 1080, bottom: 1440, left: 900, right: 900 } };
const TWO = { count: 2, space: 400, equalWidth: true };

const doc = new Document({
  creator: 'Fida Shameer, Avanti Golatkar',
  title: 'Predicting and Recovering from Distributed Deadlocks under Partial Observation',
  sections: [
    { properties: { page: PAGE }, children: head },
    { properties: { page: PAGE, type: SectionType.CONTINUOUS, column: TWO }, children: A },
    { properties: { page: PAGE, type: SectionType.CONTINUOUS, column: { count: 1 } }, children: G },
    { properties: { page: PAGE, type: SectionType.CONTINUOUS, column: TWO }, children: M },
    { properties: { page: PAGE, type: SectionType.CONTINUOUS, column: { count: 1 } }, children: H },
    { properties: { page: PAGE, type: SectionType.CONTINUOUS, column: TWO }, children: B },
  ],
});

Packer.toBuffer(doc).then(x => {
  fs.writeFileSync(path.join(__dirname, 'IEEE_Paper.docx'), x);
  console.log('written', x.length);
});
