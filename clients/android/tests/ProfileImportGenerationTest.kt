package app.skirk.client

fun main() {
    val imports = ProfileImportGeneration()
    val applied = mutableListOf<String>()
    fun completion(request: ProfileImportGeneration.Request, target: String, value: String) = {
        if (imports.owns(request, target)) applied.add(value)
    }
    val slowA = imports.begin("new")
    val finishA = completion(slowA, "new", "A")
    val newerB = imports.begin("new")
    completion(newerB, "new", "B")()
    finishA()
    check(applied == listOf("B"))
    val replaceA = imports.begin("replace:A")
    completion(replaceA, "replace:B", "wrong target")()
    check(applied == listOf("B"))
    val pickerA = imports.begin("replace:A")
    imports.cancel() // Dialog target changed while the external picker was open.
    imports.begin("replace:B")
    check(!imports.owns(pickerA, "replace:B"))
    check(!imports.owns(pickerA, "replace:A"))
    val destroyed = imports.begin("new")
    imports.cancel()
    completion(destroyed, "new", "after destroy/cancel")()
    check(applied == listOf("B"))
    println("Profile import generation regression passed")
}
