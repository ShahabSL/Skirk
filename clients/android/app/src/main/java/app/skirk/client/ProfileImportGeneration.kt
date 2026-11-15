package app.skirk.client

// Accessed only by the UI owner. Background readers return data; they never
// decide whether the selected import target still owns the result.
class ProfileImportGeneration {
    data class Request internal constructor(val generation: Long, val target: String)
    private var generation = 0L
    private var picker: Request? = null
    fun begin(target: String): Request = Request(++generation, target)
    fun owns(request: Request, target: String): Boolean = request.generation == generation && request.target == target
    // ActivityResult supplies no operation ID. Allow only one outstanding OS
    // picker; cancellation invalidates its generation but cannot dismiss it.
    fun beginPicker(target: String): Request? {
        if (picker != null) return null
        return begin(target).also { picker = it }
    }
    fun finishPicker(replacement: Boolean): Request? {
        val pending = picker ?: return null
        if (pending.target.startsWith("replace:") != replacement) return null
        picker = null
        return pending
    }
    fun cancel() { generation++ }
}
