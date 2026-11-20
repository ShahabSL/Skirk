package app.skirk.client

import android.Manifest
import android.app.Activity
import android.content.ClipData
import android.content.ClipboardManager
import android.content.Intent
import android.content.pm.PackageManager
import android.net.Uri
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.os.PersistableBundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.consumeWindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.widthIn
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.unit.dp
import androidx.lifecycle.ViewModel
import androidx.lifecycle.ViewModelProvider
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.runInterruptible
import kotlinx.coroutines.withContext

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        val ui = ViewModelProvider(this)[SkirkScreenState::class.java]
        setContent { SkirkTheme { ConfigScreen(ui) } }
    }
}

internal enum class SkirkPage(val title: String) {
    Home("Skirk"),
    Profiles("Profiles"),
    AddProfile("Add profile"),
    ReplaceProfile("Replace profile"),
    Settings("Connection settings"),
    VpnApps("VPN apps"),
    Performance("Performance"),
    Logs("Connection logs"),
}

// Keep profile drafts and permission continuations in memory across rotation.
// Profile text must never be placed in the activity's saved-state Bundle.
internal class SkirkScreenState : ViewModel() {
    var page by mutableStateOf(SkirkPage.Home)
    var pendingDeleteProfileId by mutableStateOf<String?>(null)
    var pendingRenameProfileId by mutableStateOf<String?>(null)
    var pendingShareProfileId by mutableStateOf<String?>(null)
    var replacingProfileId by mutableStateOf<String?>(null)
    var replacementParent = SkirkPage.Home
    var logsParent = SkirkPage.Settings
    var vpnAppsParent = SkirkPage.Settings
    var profileName by mutableStateOf("")
    var rawConfig by mutableStateOf("")
    var importError by mutableStateOf("")
    var isReadingProfile by mutableStateOf(false)
    private var profileReadJob: Job? = null
    val imports = ProfileImportGeneration()
    var pendingCameraPermission: ProfileImportGeneration.Request? = null
    var scanningProfile by mutableStateOf<ProfileImportGeneration.Request?>(null)
    var pendingVpnPermission: Pair<ClientProfile, ConnectionState>? = null
    var pendingNotificationProfile: ClientProfile? = null
    var notificationPermissionResolved by mutableStateOf(false)

    fun cancelProfileRead() {
        imports.cancel()
        profileReadJob?.cancel()
        profileReadJob = null
        isReadingProfile = false
    }

    fun clearDraft() {
        cancelProfileRead()
        scanningProfile = null
        replacingProfileId = null
        profileName = ""
        rawConfig = ""
        importError = ""
    }

    fun readProfile(uri: Uri, target: String, canUpdate: () -> Boolean, read: suspend (Uri) -> String) {
        cancelProfileRead()
        val request = imports.begin(target)
        isReadingProfile = true
        profileReadJob = viewModelScope.launch {
            try {
                val text = read(uri)
                if (imports.owns(request, target) && canUpdate()) {
                    rawConfig = text
                    importError = ""
                }
            } catch (cancelled: CancellationException) {
                throw cancelled
            } catch (error: Exception) {
                if (imports.owns(request, target) && canUpdate()) {
                    importError = (error as? IllegalArgumentException)?.message
                        ?: "Could not read the selected file. Choose it again."
                }
            } finally {
                if (imports.owns(request, target)) isReadingProfile = false
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ConfigScreen(ui: SkirkScreenState) {
    val context = LocalContext.current
    val appContext = context.applicationContext
    val clipboard = LocalClipboardManager.current
    val store = remember(appContext) { ProfileStore(appContext) }
    val connectionStore = remember(appContext) { ConnectionStateStore(appContext) }
    var profiles by remember { mutableStateOf(store.listProfiles()) }
    var selectedId by remember { mutableStateOf(store.selectedProfileId()) }
    var connection by remember { mutableStateOf(connectionStore.read()) }
    var metrics by remember { mutableStateOf<DriveMetrics?>(null) }
    var logs by remember { mutableStateOf("") }
    var exitCheck by remember(connection) { mutableStateOf<ExitCheck>(ExitCheck.Unchecked) }
    var exitCheckJob by remember(connection) { mutableStateOf<Job?>(null) }
    val scope = rememberCoroutineScope()
    val snackbar = remember { SnackbarHostState() }
    val selected = profiles.firstOrNull { it.id == selectedId } ?: profiles.firstOrNull()
    // The service owns the active connection, even if the saved selection differs.
    val active = profiles.firstOrNull { it.id == connection.profileId }
    val panelProfile = if (connection.running) active else selected
    val panelConnection = connection.forProfile(panelProfile?.id)
    val names = remember(profiles) { displayProfileNames(profiles) }
    DisposableEffect(connection) {
        onDispose { exitCheckJob?.cancel() }
    }

    fun refreshProfiles() {
        profiles = store.listProfiles()
        selectedId = store.selectedProfileId()
    }

    fun acceptConnection(next: ConnectionState) {
        if (next != connection) {
            if (next.invalidatesDriveMetrics(connection)) metrics = null
            connection = next
        }
    }

    DisposableEffect(connectionStore) {
        val unsubscribe = connectionStore.observeChanges { acceptConnection(connectionStore.read()) }
        acceptConnection(connectionStore.read())
        onDispose { unsubscribe() }
    }

    fun refreshConnection() {
        val previous = connectionStore.read()
        if (previous.running && System.currentTimeMillis() - previous.updatedAtMillis > SERVICE_STATE_GRACE_MS &&
            !isSkirkServiceRunning(appContext, previous.mode)
        ) {
            connectionStore.finish(previous, "Disconnected")
        }
        acceptConnection(connectionStore.read())
    }

    fun refreshLogs() {
        val mode = connectionStore.read().mode
        scope.launch {
            val text = withContext(Dispatchers.IO) { runCatching { readSkirkLogs(appContext, mode) }.getOrDefault("") }
            if (mode == connectionStore.read().mode) logs = text
        }
    }

    fun updateSettings(change: (ClientProfile) -> ClientProfile) {
        if (!connectionStore.read().running) {
            store.selectedProfile()?.let { store.saveProfile(change(it)) }
            refreshProfiles()
        }
    }

    fun disconnect() {
        val sent = connectionStore.requestDisconnect { pending ->
            when (pending.mode) {
                ClientProfile.CONNECTION_MODE_VPN -> SkirkVpnService.stop(context, pending)
                ClientProfile.CONNECTION_MODE_PROXY -> SkirkProxyService.stop(context, pending)
                else -> false
            }
        }
        refreshConnection()
        if (!sent) scope.launch { snackbar.showSnackbar("Couldn’t request disconnect. Try again.") }
    }

    fun startVpn(profile: ClientProfile) {
        SkirkVpnService.start(context, profile)
        refreshConnection()
    }

    val vpnPermission = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
        val pending = ui.pendingVpnPermission
        ui.pendingVpnPermission = null
        if (pending != null) {
            val (profile, previous) = pending
            if (result.resultCode == Activity.RESULT_OK) {
                if (connectionStore.read() == previous) startVpn(profile)
            } else if (connectionStore.finish(previous, "VPN permission was not granted")) {
                refreshConnection()
                scope.launch { snackbar.showSnackbar("Allow the Android VPN request to connect.") }
            }
        }
    }
    val notificationPermission = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {
        ui.notificationPermissionResolved = true
    }

    fun connect(profile: ClientProfile) {
        if (connectionStore.read().running || ui.pendingVpnPermission != null || ui.pendingNotificationProfile != null) return
        ui.cancelProfileRead()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU && !ui.notificationPermissionResolved &&
            context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            ui.pendingNotificationProfile = profile
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
            return
        }
        if (profile.connectionMode == ClientProfile.CONNECTION_MODE_VPN) {
            val runtimeProfile = profile.copy(shareLan = false)
            if (runtimeProfile != profile) {
                store.saveProfile(runtimeProfile)
                refreshProfiles()
            }
            val consent = VpnService.prepare(context)
            if (consent == null) startVpn(runtimeProfile)
            else {
                val previous = connectionStore.read()
                if (previous.running) return
                ui.pendingVpnPermission = runtimeProfile to previous
                vpnPermission.launch(consent)
            }
        } else {
            SkirkProxyService.start(context, profile)
            refreshConnection()
        }
    }

    LaunchedEffect(ui.notificationPermissionResolved) {
        if (ui.notificationPermissionResolved) {
            val profile = ui.pendingNotificationProfile
            ui.pendingNotificationProfile = null
            profile?.let(::connect)
        }
    }
    LaunchedEffect(Unit) {
        while (true) {
            refreshConnection()
            val current = connection
            if (current.canShowDriveMetrics) {
                val measurement = withContext(Dispatchers.IO) { readDriveMetrics(appContext, current) }
                val actual = connectionStore.read()
                if (actual.sharesLiveDiagnostics(current)) {
                    metrics = measurement
                } else {
                    refreshConnection()
                }
            }
            delay(2_000L)
        }
    }
    LaunchedEffect(ui.page, connection.mode) {
        if (ui.page == SkirkPage.Logs) refreshLogs()
    }

    fun profileImportTarget(): String? = when (ui.page) {
        SkirkPage.Home -> "new".takeIf { store.listProfiles().isEmpty() }
        SkirkPage.AddProfile -> "new"
        SkirkPage.ReplaceProfile -> ui.replacingProfileId?.let { "replace:$it" }
        else -> null
    }
    fun ownsProfileImport(request: ProfileImportGeneration.Request): Boolean =
        request.target == profileImportTarget() && ui.imports.owns(request, request.target) &&
            !connectionStore.read().running

    fun pickedFile(uri: Uri?, replacement: Boolean) {
        val request = ui.imports.finishPicker(replacement) ?: return
        if (uri == null || !ownsProfileImport(request)) return
        ui.readProfile(uri, request.target, {
            profileImportTarget() == request.target && !connectionStore.read().running
        }) { readProfileDocument(appContext, it) }
    }
    val profilePicker = rememberLauncherForActivityResult(ActivityResultContracts.GetContent()) {
        pickedFile(it, replacement = false)
    }
    val replacementPicker = rememberLauncherForActivityResult(ActivityResultContracts.GetContent()) {
        pickedFile(it, replacement = true)
    }
    val cameraPermission = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted ->
        val request = ui.pendingCameraPermission
        ui.pendingCameraPermission = null
        if (request != null && ownsProfileImport(request)) {
            if (granted) ui.scanningProfile = request
            else ui.importError = "Allow camera access in Android Settings to scan a QR code, or paste the profile."
        }
    }
    val cameraAvailable = remember(context) {
        context.packageManager.hasSystemFeature(PackageManager.FEATURE_CAMERA_ANY)
    }
    fun scanProfile() {
        val target = profileImportTarget() ?: return
        if (ui.pendingCameraPermission != null || connectionStore.read().running) return
        ui.cancelProfileRead()
        ui.importError = ""
        val request = ui.imports.begin(target)
        if (context.checkSelfPermission(Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED) {
            ui.scanningProfile = request
        } else {
            ui.pendingCameraPermission = request
            cameraPermission.launch(Manifest.permission.CAMERA)
        }
    }
    fun finishProfileScan(request: ProfileImportGeneration.Request): Boolean {
        if (ui.scanningProfile !== request) return false
        ui.scanningProfile = null
        return ownsProfileImport(request)
    }

    fun pasteProfile() {
        ui.cancelProfileRead()
        val manager = context.getSystemService(ClipboardManager::class.java)
        val text = manager?.primaryClip?.takeIf { it.itemCount > 0 }?.getItemAt(0)?.text
        ui.importError = when {
            text != null && SkirkConfig.isOversizedDraft(text.length) -> PROFILE_TEXT_TOO_LARGE
            text.isNullOrBlank() -> "Copy the profile text and try again, or choose a profile file."
            else -> {
                ui.rawConfig = text.toString()
                ""
            }
        }
    }
    fun saveProfile() {
        ui.cancelProfileRead()
        val previous = connectionStore.read()
        if (previous.running) {
            ui.importError = "Disconnect before changing profiles."
            return
        }
        try {
            val replacing = ui.replacingProfileId
            if (replacing != null) {
                val original = profiles.firstOrNull { it.id == replacing }
                    ?: error("This profile was removed. Try adding a new profile.")
                store.saveProfile(original.withRenewedConfig(ui.rawConfig))
                if (previous.profileId == replacing) connectionStore.finish(previous, "Ready to connect")
                ui.page = ui.replacementParent
            } else {
                val name = ui.profileName.trim().ifBlank { nextProfileName(profiles) }
                val profile = ClientProfile.fromRawConfig(
                    name = name,
                    rawConfig = ui.rawConfig,
                    socksPort = ClientProfile.DEFAULT_SOCKS_PORT,
                    httpPort = ClientProfile.DEFAULT_HTTP_PORT,
                    shareLan = false,
                    connectionMode = ClientProfile.CONNECTION_MODE_VPN,
                ).copy(routeMode = ClientProfile.ROUTE_GOOGLE_FRONT)
                store.saveProfile(profile, makeSelected = true)
                ui.page = SkirkPage.Home
            }
            ui.clearDraft()
            refreshProfiles()
            refreshConnection()
            scope.launch { snackbar.showSnackbar(if (replacing == null) "Profile saved" else "Profile updated") }
        } catch (error: Exception) {
            ui.importError = error.message ?: "Could not save the profile. Check the profile text."
        }
    }
    fun openReplacement(profile: ClientProfile) {
        ui.clearDraft()
        ui.replacementParent = ui.page
        ui.replacingProfileId = profile.id
        ui.page = SkirkPage.ReplaceProfile
    }
    fun up() {
        val parent = when (ui.page) {
            SkirkPage.Profiles, SkirkPage.Settings -> SkirkPage.Home
            SkirkPage.AddProfile -> SkirkPage.Profiles
            SkirkPage.ReplaceProfile -> ui.replacementParent
            SkirkPage.VpnApps -> ui.vpnAppsParent
            SkirkPage.Performance -> SkirkPage.Settings
            SkirkPage.Logs -> ui.logsParent
            SkirkPage.Home -> SkirkPage.Home
        }
        if (ui.page == SkirkPage.AddProfile || ui.page == SkirkPage.ReplaceProfile) ui.clearDraft()
        ui.page = parent
    }
    BackHandler(enabled = ui.page != SkirkPage.Home, onBack = ::up)

    val profileForm: @Composable () -> Unit = {
        ProfileForm(
            replacingName = ui.replacingProfileId?.let(names::get),
            profileName = ui.profileName,
            rawConfig = ui.rawConfig,
            error = ui.importError,
            reading = ui.isReadingProfile,
            enabled = !connection.running,
            showHeading = ui.page == SkirkPage.Home,
            onNameChange = { ui.profileName = it },
            onTextChange = {
                ui.cancelProfileRead()
                if (SkirkConfig.isOversizedDraft(it.length)) {
                    ui.importError = PROFILE_TEXT_TOO_LARGE
                } else {
                    ui.rawConfig = it
                    ui.importError = ""
                }
            },
            onPaste = ::pasteProfile,
            onScanQr = if (cameraAvailable) ::scanProfile else null,
            onChooseFile = {
                ui.cancelProfileRead()
                profileImportTarget()?.let { target ->
                    ui.imports.beginPicker(target)?.let {
                        if (ui.replacingProfileId == null) profilePicker.launch("*/*")
                        else replacementPicker.launch("*/*")
                    }
                }
            },
            onCancelRead = ui::cancelProfileRead,
            onSave = ::saveProfile,
        )
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        contentWindowInsets = WindowInsets.safeDrawing,
        snackbarHost = { SnackbarHost(snackbar) },
        topBar = { SkirkTopBar(ui.page, ::up) },
    ) { insets ->
        Box(
            modifier = Modifier.fillMaxSize().padding(insets).consumeWindowInsets(insets).imePadding(),
            contentAlignment = Alignment.TopCenter,
        ) {
            Box(Modifier.widthIn(max = if (ui.page == SkirkPage.Home && panelProfile != null) 860.dp else 640.dp)
                .fillMaxWidth().fillMaxSize()) {
                when (ui.page) {
                    SkirkPage.Home -> if (profiles.isEmpty() && !connection.running) profileForm() else {
                        ConnectionHome(
                            profile = panelProfile,
                            profileName = panelProfile?.id?.let(names::get),
                            connection = panelConnection,
                            backgroundFailureName = active?.id?.takeIf {
                                connection.phase == ConnectionPhase.FAILED && it != panelProfile?.id
                            }?.let(names::get),
                            metrics = metrics,
                            exitCheck = exitCheck,
                            onCheckExit = {
                                val profile = active
                                if (profile != null && exitCheck != ExitCheck.Checking && connection.canShowDriveMetrics) {
                                    val attempt = connection
                                    if (!connectionStore.read().sharesLiveDiagnostics(attempt)) {
                                        refreshConnection()
                                        return@ConnectionHome
                                    }
                                    exitCheck = ExitCheck.Checking
                                    exitCheckJob = scope.launch {
                                        val result = try {
                                            val result = runInterruptible(Dispatchers.IO) { probeExit(appContext, profile) }
                                            if (result.verified) ExitCheck.Replied(result.roundTripMs) else ExitCheck.NoReply
                                        } catch (cancelled: CancellationException) {
                                            throw cancelled
                                        } catch (_: Exception) {
                                            ExitCheck.CouldNotCheck
                                        }
                                        if (connectionStore.read().sharesLiveDiagnostics(attempt)) exitCheck = result
                                        else refreshConnection()
                                    }
                                }
                            },
                            onConnect = { selected?.let(::connect) },
                            onDisconnect = ::disconnect,
                            onReplace = { selected?.let(::openReplacement) },
                            onOpenProfiles = { ui.page = SkirkPage.Profiles },
                            onOpenSettings = { ui.page = SkirkPage.Settings },
                            onOpenVpnApps = { ui.vpnAppsParent = SkirkPage.Home; ui.page = SkirkPage.VpnApps },
                            onOpenLogs = { ui.logsParent = SkirkPage.Home; ui.page = SkirkPage.Logs },
                        )
                    }
                    SkirkPage.Profiles -> ProfilesPage(
                        profiles = profiles,
                        names = names,
                        currentId = panelProfile?.id,
                        running = connection.running,
                        onSelect = { profile ->
                            if (!connectionStore.read().running) {
                                store.selectProfile(profile.id)
                                refreshProfiles()
                                ui.page = SkirkPage.Home
                            }
                        },
                        onAdd = { ui.clearDraft(); ui.page = SkirkPage.AddProfile },
                        onReplace = ::openReplacement,
                        onRename = { ui.pendingRenameProfileId = it.id },
                        onRemove = { ui.pendingDeleteProfileId = it.id },
                        onShare = { ui.pendingShareProfileId = it.id },
                    )
                    SkirkPage.AddProfile, SkirkPage.ReplaceProfile -> profileForm()
                    SkirkPage.Settings -> panelProfile?.let { profile ->
                        SettingsPage(
                            profile = profile,
                            running = connection.running,
                            onModeChange = { mode -> updateSettings { it.copy(connectionMode = mode) } },
                            onRouteModeChange = { route -> updateSettings { it.copy(routeMode = route) } },
                            onProxyShareLanChange = { share -> updateSettings { it.copy(shareLan = share) } },
                            onProxyPortsChange = { socks, http -> updateSettings { it.copy(socksPort = socks, httpPort = http) } },
                            onOpenVpnApps = { ui.vpnAppsParent = SkirkPage.Settings; ui.page = SkirkPage.VpnApps },
                            onOpenPerformance = { ui.page = SkirkPage.Performance },
                            onOpenLogs = { ui.logsParent = SkirkPage.Settings; ui.page = SkirkPage.Logs },
                        )
                    }
                    SkirkPage.VpnApps -> selected?.let { profile ->
                        VpnAppsPage(profile, enabled = !connection.running, onSave = { mode, allowed, excluded ->
                            val attempt = connectionStore.read()
                            updateSettings { it.copy(vpnAppMode = mode, vpnAllowedApps = allowed, vpnExcludedApps = excluded) }
                            if (attempt.phase == ConnectionPhase.FAILED && attempt.profileId == profile.id &&
                                isVpnAppSelectionFailure(attempt.message)
                            ) {
                                connectionStore.finish(attempt, "Ready to connect")
                                refreshConnection()
                            }
                            ui.page = ui.vpnAppsParent
                        })
                    }
                    SkirkPage.Performance -> panelProfile?.let { profile ->
                        PerformancePage(profile.performance, metrics, enabled = !connection.running, onPerformanceChange = {
                            updateSettings { current -> current.copy(performance = it) }
                        })
                    }
                    SkirkPage.Logs -> LogsPage(logs, onRefresh = ::refreshLogs, onCopy = {
                        clipboard.setText(AnnotatedString(logs))
                        scope.launch { snackbar.showSnackbar("Logs copied") }
                    })
                }
            }
        }
    }

    ui.scanningProfile?.let { request ->
        ProfileQrScanner(
            onDismiss = { finishProfileScan(request) },
            onCameraError = {
                if (finishProfileScan(request)) {
                    ui.importError = "Could not open the camera. Try again or paste the profile."
                }
            },
            onCode = { text ->
                if (finishProfileScan(request)) {
                    val scanned = SkirkConfig.scannedProfile(text)
                    if (scanned == null) {
                        ui.importError = "Scan a Skirk profile QR code or paste the profile."
                    } else {
                        ui.rawConfig = scanned
                        ui.importError = ""
                    }
                }
            },
        )
    }
    profiles.firstOrNull { it.id == ui.pendingShareProfileId }?.let { profile ->
        ProfileShareDialog(
            rawConfig = profile.rawConfig,
            onDismiss = { ui.pendingShareProfileId = null },
            onCopy = {
                val clip = ClipData.newPlainText("Skirk profile", it)
                clip.description.extras = PersistableBundle().apply {
                    // ClipDescription.EXTRA_IS_SENSITIVE on API 33 and later.
                    putBoolean("android.content.extra.IS_SENSITIVE", true)
                }
                context.getSystemService(ClipboardManager::class.java).setPrimaryClip(clip)
            },
            onShare = {
                val send = Intent(Intent.ACTION_SEND).apply {
                    type = "text/plain"
                    putExtra(Intent.EXTRA_TEXT, it)
                }
                ui.pendingShareProfileId = null
                context.startActivity(Intent.createChooser(send, "Share Skirk profile"))
            },
        )
    }
    profiles.firstOrNull { it.id == ui.pendingRenameProfileId }?.let { profile ->
        RenameProfileDialog(
            name = profile.name,
            onDismiss = { ui.pendingRenameProfileId = null },
            onSave = { name ->
                if (!connectionStore.read().running) {
                    store.saveProfile(profile.copy(name = name))
                    refreshProfiles()
                }
                ui.pendingRenameProfileId = null
            },
        )
    }
    profiles.firstOrNull { it.id == ui.pendingDeleteProfileId }?.let { profile ->
        AlertDialog(
            onDismissRequest = { ui.pendingDeleteProfileId = null },
            title = { Text("Remove profile?") },
            text = { Text("Remove ${names.getValue(profile.id)} from this device. You’ll need the original profile to add it again.") },
            confirmButton = {
                TextButton(onClick = {
                    if (!connectionStore.read().running) {
                        store.deleteProfile(profile.id)
                        refreshProfiles()
                        if (profiles.isEmpty()) ui.page = SkirkPage.Home
                    }
                    ui.pendingDeleteProfileId = null
                }) { Text("Remove", color = MaterialTheme.colorScheme.error) }
            },
            dismissButton = { TextButton(onClick = { ui.pendingDeleteProfileId = null }) { Text("Cancel") } },
        )
    }
}
