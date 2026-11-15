package app.skirk.client

// Accessed only by the UI owner. Background readers return data; they never
// decide whether the selected import target still owns the result.
class ProfileImportGeneration {
    data class Request internal constructor(val generation: Long, val target: String)
    private var generation = 0L
    fun begin(target: String): Request = Request(++generation, target)
    fun owns(request: Request, target: String): Boolean = request.generation == generation && request.target == target
    fun cancel() { generation++ }
}
