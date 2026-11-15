export type SnapshotAction = "connect" | "disconnect" | "import" | "select" | "delete" | "mode" | "performance" | "route";
export type SnapshotRead = Readonly<{ epoch: number; id: number }>;
export type SnapshotMutation = Readonly<{ id: number; action: SnapshotAction }>;

// A mutation result is only an acknowledgement: render a fresh read afterwards.
// At most one current read is allowed, preventing overlapping slow polls from
// starving every response. Mutation boundaries invalidate older in-flight reads.
export class SnapshotGuard {
  private epoch = 0;
  private nextID = 0;
  private read: SnapshotRead | null = null;
  private mutation: SnapshotMutation | null = null;

  beginRead(): SnapshotRead | null {
    if (this.read) return null;
    this.read = { epoch: this.epoch, id: ++this.nextID };
    return this.read;
  }

  acceptsRead(read: SnapshotRead): boolean {
    return read.epoch === this.epoch && read.id === this.read?.id;
  }

  finishRead(read: SnapshotRead): void {
    if (this.acceptsRead(read)) this.read = null;
  }

  beginMutation(action: SnapshotAction): SnapshotMutation | null {
    if (this.mutation && !(action === "disconnect" && this.mutation.action === "connect")) return null;
    this.invalidateReads();
    this.mutation = { id: ++this.nextID, action };
    return this.mutation;
  }

  ownsMutation(mutation: SnapshotMutation): boolean {
    return mutation.id === this.mutation?.id;
  }

  settleMutation(mutation: SnapshotMutation): boolean {
    if (!this.ownsMutation(mutation)) return false;
    this.invalidateReads();
    return true;
  }

  finishMutation(mutation: SnapshotMutation): boolean {
    if (!this.ownsMutation(mutation)) return false;
    this.mutation = null;
    return true;
  }

  reset(): void {
    this.invalidateReads();
    this.mutation = null;
  }

  private invalidateReads(): void {
    this.epoch += 1;
    this.read = null;
  }
}
