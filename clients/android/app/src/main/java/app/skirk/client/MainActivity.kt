package app.skirk.client

import android.Manifest
import android.app.Activity
import android.app.ActivityManager
import android.content.pm.PackageManager
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.view.View
import android.widget.Toast
import androidx.activity.ComponentActivity
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.selection.selectable
import androidx.compose.foundation.selection.selectableGroup
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.rounded.Add
import androidx.compose.material.icons.rounded.AccountCircle
import androidx.compose.material.icons.rounded.Check
import androidx.compose.material.icons.rounded.CloudQueue
import androidx.compose.material.icons.rounded.ContentCopy
import androidx.compose.material.icons.rounded.ContentPaste
import androidx.compose.material.icons.rounded.DataUsage
import androidx.compose.material.icons.rounded.Delete
import androidx.compose.material.icons.rounded.PlayArrow
import androidx.compose.material.icons.rounded.PowerSettingsNew
import androidx.compose.material.icons.rounded.Refresh
import androidx.compose.material.icons.rounded.Settings
import androidx.compose.material.icons.rounded.Shield
import androidx.compose.material.icons.rounded.Speed
import androidx.compose.material.icons.rounded.Storage
import androidx.compose.material.icons.rounded.Tune
import androidx.compose.material.icons.rounded.Visibility
import androidx.compose.material.icons.rounded.VisibilityOff
import androidx.compose.material.icons.rounded.VpnKey
import androidx.compose.material.icons.rounded.WifiTethering
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.SwitchDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.material3.TopAppBarDefaults
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.SideEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.mutableIntStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.toArgb
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalView
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.clearAndSetSemantics
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.heading
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.onClick
import androidx.compose.ui.semantics.role
import androidx.compose.ui.semantics.stateDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Job
import kotlinx.coroutines.delay
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File
import org.json.JSONObject

private val LightColors = lightColorScheme(
    primary = Color(0xFF111111),
    onPrimary = Color.White,
    surface = Color.White,
    background = Color(0xFFF6F6F6),
    onSurface = Color(0xFF111111),
    surfaceVariant = Color(0xFFF4F4F5),
    onSurfaceVariant = Color(0xFF71717A),
    outline = Color(0xFFE4E4E7),
)

private val DarkColors = darkColorScheme(
    primary = Color(0xFFF5F5F5),
    onPrimary = Color(0xFF111111),
    surface = Color(0xFF252526),
    background = Color(0xFF1E1E1E),
    onSurface = Color(0xFFF5F5F5),
    surfaceVariant = Color(0xFF2D2D30),
    onSurfaceVariant = Color(0xFFA7A7AD),
    outline = Color(0xFF3C3C3C),
)

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        setContent {
            SkirkTheme {
                ConfigScreen()
            }
        }
    }
}

@Composable
@Suppress("DEPRECATION")
private fun SkirkTheme(content: @Composable () -> Unit) {
    val dark = isSystemInDarkTheme()
    val colors = if (dark) DarkColors else LightColors
    val view = LocalView.current
    SideEffect {
        val window = (view.context as Activity).window
        window.statusBarColor = colors.background.toArgb()
        window.navigationBarColor = colors.background.toArgb()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            window.decorView.systemUiVisibility = if (dark) {
                0
            } else {
                View.SYSTEM_UI_FLAG_LIGHT_STATUS_BAR
            }
        }
    }
    MaterialTheme(colorScheme = colors, content = content)
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ConfigScreen() {
    val context = LocalContext.current
    val clipboard = LocalClipboardManager.current
    val store = remember(context) { ProfileStore(context.applicationContext) }
    val connectionStore = remember(context) { ConnectionStateStore(context.applicationContext) }
    var profiles by remember { mutableStateOf(store.listProfiles()) }
    var selectedId by remember { mutableStateOf(store.selectedProfileId()) }
    var connectionState by remember { mutableStateOf(connectionStore.read()) }
    var lastConnectionUpdateAt by remember { mutableStateOf(connectionState.updatedAtMillis) }
    var rawConfig by remember { mutableStateOf("") }
    var importError by remember { mutableStateOf("") }
    var profileName by remember { mutableStateOf("Skirk profile") }
    var socksPort by remember { mutableStateOf(ClientProfile.DEFAULT_SOCKS_PORT.toString()) }
    var httpPort by remember { mutableStateOf(ClientProfile.DEFAULT_HTTP_PORT.toString()) }
    var selectedMode by remember { mutableStateOf(ClientProfile.CONNECTION_MODE_VPN) }
    var proxyShareLan by remember { mutableStateOf(false) }
    val running = connectionState.running
    var message by remember { mutableStateOf(connectionState.message) }
    var logText by remember { mutableStateOf(readSkirkLogs(context, connectionState.mode)) }
    var driveMetrics by remember { mutableStateOf(readDriveMetrics(context, connectionState.mode)) }
    var diagnosticsExpanded by remember { mutableStateOf(false) }
    var pendingVpnProfile by remember { mutableStateOf<ClientProfile?>(null) }
    var pendingNotificationStart by remember { mutableStateOf<Triple<ClientProfile, String, Boolean>?>(null) }
    var notificationPermissionResolved by remember { mutableStateOf(false) }
    var importExpanded by remember { mutableStateOf(profiles.isEmpty()) }
    var replacingProfileId by remember { mutableStateOf<String?>(null) }
    var pendingDeleteProfile by remember { mutableStateOf<ClientProfile?>(null) }
    var replacementConfig by remember { mutableStateOf("") }
    var replacementError by remember { mutableStateOf("") }
    var advancedSettingsOpen by remember { mutableStateOf(false) }
    var vpnAppsDialogOpen by remember { mutableStateOf(false) }
    var exitProbeStatus by remember { mutableStateOf("") }
    var exitProbeRunning by remember { mutableStateOf(false) }
    var exitProbeGeneration by remember { mutableIntStateOf(0) }
    val coroutineScope = rememberCoroutineScope()
    val profileImports = remember { ProfileImportGeneration() }
    var profileReadJob by remember { mutableStateOf<Job?>(null) }
    var profileReadTarget by remember { mutableStateOf<String?>(null) }

    fun cancelProfileRead() {
        profileImports.cancel()
        profileReadJob?.cancel()
        profileReadJob = null
        profileReadTarget = null
    }
    DisposableEffect(Unit) {
        onDispose { cancelProfileRead() }
    }
    fun readPickedProfile(uri: Uri, target: String) {
        cancelProfileRead()
        val request = profileImports.begin(target)
        profileReadTarget = target
        profileReadJob = coroutineScope.launch {
            try {
                val text = readProfileDocument(context.applicationContext, uri)
                val currentTarget = if (target == "new") "new" else replacingProfileId?.let { "replace:$it" }.orEmpty()
                if (profileImports.owns(request, currentTarget) && !connectionStore.read().running) {
                    if (target == "new") { rawConfig = text; importError = "" }
                    else { replacementConfig = text; replacementError = "" }
                }
            } catch (_: CancellationException) {
                // Replaced or destroyed imports never update the next target.
            } catch (error: Exception) {
                val currentTarget = if (target == "new") "new" else replacingProfileId?.let { "replace:$it" }.orEmpty()
                if (profileImports.owns(request, currentTarget) && !connectionStore.read().running) {
                    val detail = if (error is IllegalArgumentException) error.message else null
                    val failure = detail ?: "Could not read the selected file. Choose it again."
                    if (target == "new") importError = failure else replacementError = failure
                }
            } finally {
                if (profileImports.owns(request, target)) { profileReadTarget = null; profileReadJob = null }
            }
        }
    }

    LaunchedEffect(connectionState.running, connectionState.profileId) {
        exitProbeGeneration += 1
        exitProbeStatus = ""
        exitProbeRunning = false
    }

    fun refreshConnectionState() {
        val raw = connectionStore.read()
        val oldEnoughToValidate = System.currentTimeMillis() - raw.updatedAtMillis > SERVICE_STATE_GRACE_MS
        if (raw.running && oldEnoughToValidate && !isSkirkServiceRunning(context, raw.mode)) {
            connectionStore.stopped("Disconnected")
        }
        val next = connectionStore.read()
        connectionState = next
        if (next.updatedAtMillis > lastConnectionUpdateAt) {
            lastConnectionUpdateAt = next.updatedAtMillis
            if (next.message.isNotBlank()) {
                message = next.message
            }
        }
    }

    fun refreshLogs(mode: String = connectionStore.read().mode) {
        logText = readSkirkLogs(context, mode)
        driveMetrics = readDriveMetrics(context, mode)
    }

    val notificationPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.RequestPermission(),
    ) { notificationPermissionResolved = true }
    val vpnPermission = rememberLauncherForActivityResult(
        ActivityResultContracts.StartActivityForResult(),
    ) { result ->
        val profile = pendingVpnProfile
        pendingVpnProfile = null
        if (result.resultCode == Activity.RESULT_OK && profile != null) {
            if (connectionStore.read().mode == ClientProfile.CONNECTION_MODE_PROXY) {
                SkirkProxyService.stop(context)
            }
            connectionStore.connecting(profile, "VPN connecting")
            refreshConnectionState()
            SkirkVpnService.start(context, profile)
            message = "VPN connecting"
        } else {
            connectionStore.stopped("VPN permission was not granted")
            refreshConnectionState()
            message = "VPN permission was not granted"
        }
    }
    val replacementFilePicker = rememberLauncherForActivityResult(ActivityResultContracts.GetContent()) { uri ->
        val request = profileImports.finishPicker(replacement = true)
        val target = replacingProfileId?.let { "replace:$it" }.orEmpty()
        if (uri != null && request != null && request.target.startsWith("replace:") &&
            profileImports.owns(request, target) && !connectionStore.read().running) {
            readPickedProfile(uri, target)
        }
    }
    val newProfileFilePicker = rememberLauncherForActivityResult(ActivityResultContracts.GetContent()) { uri ->
        val request = profileImports.finishPicker(replacement = false)
        if (uri != null && request != null && request.target == "new" &&
            profileImports.owns(request, "new") && !connectionStore.read().running) readPickedProfile(uri, "new")
    }

    fun refresh() {
        profiles = store.listProfiles()
        selectedId = store.selectedProfileId()
    }

    fun updateSelectedProfile(change: (ClientProfile) -> ClientProfile) {
        store.selectedProfile()?.let { current ->
            store.saveProfile(change(current))
            refresh()
        }
    }

    fun startProfile(profile: ClientProfile, mode: String, shareLan: Boolean) {
        cancelProfileRead()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            !notificationPermissionResolved &&
            context.checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            pendingNotificationStart = Triple(profile, mode, shareLan)
            notificationPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
            return
        }
        val normalizedMode = ClientProfile.normalizeConnectionMode(mode)
        val runtimeProfile = profile.copy(
            connectionMode = normalizedMode,
            shareLan = normalizedMode == ClientProfile.CONNECTION_MODE_PROXY && shareLan,
        )
        store.saveProfile(runtimeProfile)
        refresh()
        if (runtimeProfile.connectionMode == ClientProfile.CONNECTION_MODE_VPN) {
            if (connectionStore.read().mode == ClientProfile.CONNECTION_MODE_PROXY) {
                SkirkProxyService.stop(context)
            }
            val intent = VpnService.prepare(context)
            if (intent != null) {
                pendingVpnProfile = runtimeProfile
                vpnPermission.launch(intent)
            } else {
                connectionStore.connecting(runtimeProfile, "VPN connecting")
                refreshConnectionState()
                SkirkVpnService.start(context, runtimeProfile)
                message = "VPN connecting"
            }
        } else {
            if (connectionStore.read().mode == ClientProfile.CONNECTION_MODE_VPN) {
                SkirkVpnService.stop(context)
            }
            connectionStore.connecting(runtimeProfile, "Proxy connecting on ${proxyAddress(runtimeProfile, runtimeProfile.shareLan)}")
            refreshConnectionState()
            SkirkProxyService.start(context, runtimeProfile)
            message = "Proxy connecting on ${proxyAddress(runtimeProfile, runtimeProfile.shareLan)}"
        }
    }

    fun disconnectActive(reason: String = "Disconnected") {
        when (connectionStore.read().mode) {
            ClientProfile.CONNECTION_MODE_VPN -> SkirkVpnService.stop(context)
            ClientProfile.CONNECTION_MODE_PROXY -> SkirkProxyService.stop(context)
            else -> {
                SkirkVpnService.stop(context)
                SkirkProxyService.stop(context)
            }
        }
        connectionStore.stopped(reason)
        refreshConnectionState()
        message = reason
    }

    LaunchedEffect(notificationPermissionResolved) {
        if (notificationPermissionResolved) {
            val pending = pendingNotificationStart
            pendingNotificationStart = null
            pending?.let { (profile, mode, shareLan) -> startProfile(profile, mode, shareLan) }
        }
    }

    LaunchedEffect(Unit) {
        while (true) {
            refreshConnectionState()
            val next = connectionStore.read()
            if (next.running) {
                refreshLogs(next.mode)
            }
            delay(2_000L)
        }
    }

    val selected = profiles.firstOrNull { it.id == selectedId } ?: profiles.firstOrNull()
    val activeProbeProfile = profiles.firstOrNull { it.id == connectionState.profileId }
    // A restarted activity can retain a different selected profile while the
    // service is still running. Show the profile that owns the live connection.
    val panelProfile = if (running) activeProbeProfile else selected
    val panelMode = if (running) connectionState.mode.ifBlank {
        activeProbeProfile?.connectionMode ?: ClientProfile.CONNECTION_MODE_VPN
    } else selectedMode
    val panelShareLan = if (running) activeProbeProfile?.shareLan ?: false else proxyShareLan
    val panelConnection = connectionState.forProfile(panelProfile?.id)
    val otherFailure = connectionState.phase == ConnectionPhase.FAILED &&
        connectionState.profileId != panelProfile?.id
    val failureOwner = profiles.firstOrNull { it.id == connectionState.profileId }

    LaunchedEffect(selected?.id) {
        selectedMode = selected?.connectionMode ?: ClientProfile.CONNECTION_MODE_VPN
        proxyShareLan = selected?.shareLan ?: false
    }
    LaunchedEffect(profiles.isEmpty()) {
        if (profiles.isEmpty()) {
            importExpanded = true
        }
    }
    fun pasteProfileFromClipboard() {
        cancelProfileRead()
        val clipboard = context.getSystemService(ClipboardManager::class.java)
        rawConfig = clipboard.primaryClip?.getItemAt(0)?.coerceToText(context)?.toString().orEmpty()
        importError = ""
    }

    fun importProfile() {
        cancelProfileRead()
        if (running) {
            importError = "Disconnect before importing another profile."
            return
        }
        try {
            val port = socksPort.toIntOrNull()
                ?.let { ClientProfile.validateSocksPort(it) }
                ?: error("Local SOCKS port is required")
            val httpProxyPort = httpPort.toIntOrNull()
                ?.let { ClientProfile.validateHttpPort(it, port) }
                ?: error("Local HTTP proxy port is required")
            val profile = ClientProfile.fromRawConfig(
                name = profileName,
                rawConfig = rawConfig,
                socksPort = port,
                httpPort = httpProxyPort,
                shareLan = false,
                connectionMode = ClientProfile.CONNECTION_MODE_VPN,
            ).copy(routeMode = ClientProfile.ROUTE_GOOGLE_FRONT)
            store.saveProfile(profile, makeSelected = true)
            connectionStore.stopped("Ready to connect")
            refreshConnectionState()
            rawConfig = ""
            importError = ""
            selectedMode = profile.connectionMode
            proxyShareLan = false
            importExpanded = false
            message = "Imported ${profile.name}"
            refresh()
        } catch (error: Exception) {
            val nextError = error.message ?: "Import failed"
            importError = nextError
            message = nextError
            Toast.makeText(context, nextError, Toast.LENGTH_LONG).show()
        }
    }

    fun replaceProfile() {
        cancelProfileRead()
        try {
            val original = profiles.firstOrNull { it.id == replacingProfileId }
                ?: error("Profile no longer exists. Select it again to replace it.")
            val replacement = original.withRenewedConfig(replacementConfig)
            if (running) disconnectActive()
            store.saveProfile(replacement)
            connectionStore.stopped("Ready to connect")
            refreshConnectionState()
            refresh()
            selectedMode = replacement.connectionMode
            proxyShareLan = replacement.shareLan
            replacementConfig = ""
            replacementError = ""
            replacingProfileId = null
            message = "Updated ${replacement.name}. Ready to connect."
        } catch (error: Exception) {
            replacementError = error.message ?: "Could not replace profile"
        }
    }

    fun openReplacement(profile: ClientProfile) {
        cancelProfileRead()
        replacingProfileId = profile.id
        replacementConfig = ""
        replacementError = ""
    }

    val connectionPanelContent: @Composable () -> Unit = {
        if (otherFailure) {
            Text(
                failureOwner?.let { "The last connection for ${it.name} failed. Select that profile to review its recovery options." }
                    ?: "A previous connection failed. Connect the selected profile to check its status.",
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                style = MaterialTheme.typography.bodySmall,
            )
        }
        ConnectionPanel(
            selected = panelProfile,
            selectedMode = panelMode,
            proxyShareLan = panelShareLan,
            running = running,
            phase = panelConnection.phase,
            message = if (connectionState.phase == ConnectionPhase.FAILED) panelConnection.message else message,
            metrics = driveMetrics,
            onModeChange = { mode ->
                selectedMode = mode
                updateSelectedProfile { it.copy(connectionMode = mode) }
            },
            onRouteModeChange = { routeMode ->
                updateSelectedProfile { it.copy(routeMode = routeMode) }
            },
            onProxyShareLanChange = { shareLan ->
                proxyShareLan = shareLan
                updateSelectedProfile { it.copy(shareLan = shareLan) }
            },
            onOpenAdvanced = { advancedSettingsOpen = true },
            onChooseVpnApps = { vpnAppsDialogOpen = true },
            onReplaceProfile = { selected?.let(::openReplacement) },
            onCopyRenewalCommand = {
                clipboard.setText(AnnotatedString("skirk setup renew-oauth --kit skirk-kit"))
                Toast.makeText(context, "Renewal command copied", Toast.LENGTH_SHORT).show()
            },
            onCopyPersonalSetupCommand = {
                clipboard.setText(AnnotatedString("skirk setup init --out skirk-kit --reset-google-login --oauth-mode personal"))
                Toast.makeText(context, "Personal setup command copied", Toast.LENGTH_SHORT).show()
            },
            onOpenOAuthSettings = {
                context.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse("https://console.cloud.google.com/auth/audience")))
            },
            onConnect = { selected?.let { startProfile(it, selectedMode, proxyShareLan) } },
            onDisconnect = { disconnectActive() },
            exitProbeStatus = exitProbeStatus,
            exitProbeRunning = exitProbeRunning,
            canTestExit = activeProbeProfile != null,
            onTestExit = {
                val profile = activeProbeProfile
                if (profile != null && !exitProbeRunning) {
                    val generation = exitProbeGeneration
                    exitProbeRunning = true
                    exitProbeStatus = ""
                    coroutineScope.launch {
                        val result = withContext(Dispatchers.IO) {
                            runCatching { probeExit(context.applicationContext, profile) }
                        }
                        if (exitProbeGeneration == generation && connectionStore.read().running) {
                            exitProbeStatus = result.fold(
                                onSuccess = { check -> if (check.verified)
                                    "Exit responded through Google Drive in ${check.roundTripMs} ms. This is one sample; website access is not checked."
                                else
                                    "Exit could not be verified. It may be offline, older, or need Google authorization renewed. Ask the exit owner to check its status." },
                                onFailure = { "The exit test could not run. Check that the local client is still connected." },
                            )
                        }
                        if (exitProbeGeneration == generation) exitProbeRunning = false
                    }
                }
            },
        )
    }

    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            TopAppBar(
                title = {
                    Row(
                        horizontalArrangement = Arrangement.spacedBy(10.dp),
                        verticalAlignment = Alignment.CenterVertically,
                    ) {
                        Surface(
                            modifier = Modifier.size(34.dp),
                            shape = RoundedCornerShape(8.dp),
                            border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
                            color = Color.White,
                        ) {
                            Image(
                                painter = painterResource(R.drawable.logo_mark),
                                contentDescription = "Skirk",
                                contentScale = ContentScale.Fit,
                                modifier = Modifier.padding(5.dp),
                            )
                        }
                        Column {
                            Text("Skirk", fontWeight = FontWeight.SemiBold)
                            Text(
                                "v${BuildConfig.VERSION_NAME}",
                                color = MaterialTheme.colorScheme.onSurfaceVariant,
                                style = MaterialTheme.typography.labelMedium,
                            )
                        }
                    }
                },
                colors = TopAppBarDefaults.topAppBarColors(
                    containerColor = MaterialTheme.colorScheme.background,
                    titleContentColor = MaterialTheme.colorScheme.onSurface,
                    actionIconContentColor = MaterialTheme.colorScheme.onSurface,
                ),
            )
        },
    ) { innerPadding ->
        LazyColumn(
            modifier = Modifier
                .fillMaxSize()
                .padding(innerPadding)
                .padding(horizontal = 16.dp, vertical = 12.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
        ) {
            if (profiles.isNotEmpty()) {
                item { connectionPanelContent() }
            }
            if (profiles.isEmpty()) {
                item {
                    ImportPanel(
                        enabled = !running,
                        profileName = profileName,
                        socksPort = socksPort,
                        httpPort = httpPort,
                        rawConfig = rawConfig,
                        importError = importError,
                        onProfileNameChange = { profileName = it },
                        onSocksPortChange = { socksPort = it.filter(Char::isDigit).take(5) },
                        onHttpPortChange = { httpPort = it.filter(Char::isDigit).take(5) },
                        onRawConfigChange = {
                            cancelProfileRead()
                            rawConfig = it
                            importError = ""
                        },
                        onPaste = ::pasteProfileFromClipboard,
                        onChooseFile = {
                            cancelProfileRead()
                            profileImports.beginPicker("new")?.let { newProfileFilePicker.launch("*/*") }
                        },
                        readingFile = profileReadTarget == "new",
                        onCancelFile = ::cancelProfileRead,
                        onImport = ::importProfile,
                    )
                }
            } else {
                item {
                    ProfilesPanel(
                        profiles = profiles,
                        selectedId = (if (running) activeProbeProfile else selected)?.id,
                        running = running,
                        onSelect = { profile ->
                            store.selectProfile(profile.id)
                            selectedMode = profile.connectionMode
                            proxyShareLan = profile.shareLan
                            refresh()
                        },
                        onDelete = { profile ->
                            pendingDeleteProfile = profile
                        },
                        onReplace = ::openReplacement,
                    )
                }
                item {
                    if (importExpanded) {
                        ImportPanel(
                            enabled = !running,
                            profileName = profileName,
                            socksPort = socksPort,
                            httpPort = httpPort,
                            rawConfig = rawConfig,
                            importError = importError,
                            onProfileNameChange = { profileName = it },
                            onSocksPortChange = { socksPort = it.filter(Char::isDigit).take(5) },
                            onHttpPortChange = { httpPort = it.filter(Char::isDigit).take(5) },
                            onRawConfigChange = {
                                cancelProfileRead()
                                rawConfig = it
                                importError = ""
                            },
                            onPaste = ::pasteProfileFromClipboard,
                            onChooseFile = {
                                cancelProfileRead()
                                profileImports.beginPicker("new")?.let { newProfileFilePicker.launch("*/*") }
                            },
                            readingFile = profileReadTarget == "new",
                            onCancelFile = ::cancelProfileRead,
                            onImport = ::importProfile,
                        )
                    } else {
                        AddProfilePanel(onExpand = { importExpanded = true })
                    }
                }
            }

            if (profiles.isEmpty()) {
                item { connectionPanelContent() }
            }

            item {
                DiagnosticsPanel(
                    logText = logText,
                    expanded = diagnosticsExpanded,
                    onToggleExpanded = { diagnosticsExpanded = !diagnosticsExpanded },
                    onRefresh = { refreshLogs() },
                    onCopy = {
                        clipboard.setText(AnnotatedString(logText))
                        Toast.makeText(context, "Logs copied", Toast.LENGTH_SHORT).show()
                    },
                )
            }
        }
        if (advancedSettingsOpen) {
            AdvancedSettingsDialog(
                performance = selected?.performance ?: PerformanceSettings.recommended(),
                metrics = driveMetrics,
                enabled = selected != null && !running,
                onDismiss = { advancedSettingsOpen = false },
                onPerformanceChange = { performance ->
                    updateSelectedProfile { it.copy(performance = performance) }
                },
            )
        }
        if (vpnAppsDialogOpen && selected != null) {
            VpnAppsDialog(
                profile = selected,
                onDismiss = { vpnAppsDialogOpen = false },
                onSave = { mode, packages ->
                    updateSelectedProfile { it.copy(vpnAppMode = mode, vpnAllowedApps = packages) }
                    vpnAppsDialogOpen = false
                },
            )
        }
        if (replacingProfileId != null) {
            ReplaceProfileDialog(
                profileName = profiles.firstOrNull { it.id == replacingProfileId }?.name.orEmpty(),
                rawConfig = replacementConfig,
                error = replacementError,
                onRawConfigChange = {
                    cancelProfileRead()
                    replacementConfig = it
                    replacementError = ""
                },
                onPaste = {
                    cancelProfileRead()
                    val clipboardService = context.getSystemService(ClipboardManager::class.java)
                    replacementConfig = clipboardService.primaryClip?.getItemAt(0)
                        ?.coerceToText(context)?.toString().orEmpty()
                    replacementError = ""
                },
                onChooseFile = {
                    cancelProfileRead()
                    replacingProfileId?.let { target ->
                        profileImports.beginPicker("replace:$target")?.let { replacementFilePicker.launch("*/*") }
                    }
                },
                readingFile = profileReadTarget == "replace:$replacingProfileId",
                onCancelFile = ::cancelProfileRead,
                onReplace = ::replaceProfile,
                onDismiss = {
                    cancelProfileRead()
                    replacingProfileId = null
                    replacementConfig = ""
                    replacementError = ""
                },
            )
        }
        pendingDeleteProfile?.let { profile ->
            AlertDialog(
                onDismissRequest = { pendingDeleteProfile = null },
                title = { Text("Remove ${profile.name}?") },
                text = { Text("This removes the saved profile from this phone. You will need client.skirk to import it again.") },
                confirmButton = {
                    TextButton(onClick = {
                        if (!running) {
                            store.deleteProfile(profile.id)
                            refresh()
                        }
                        pendingDeleteProfile = null
                    }) { Text("Remove") }
                },
                dismissButton = {
                    TextButton(onClick = { pendingDeleteProfile = null }) { Text("Cancel") }
                },
            )
        }
    }
}


@Composable
private fun ConnectionPanel(
    selected: ClientProfile?,
    selectedMode: String,
    proxyShareLan: Boolean,
    running: Boolean,
    phase: ConnectionPhase,
    message: String,
    metrics: DriveMetrics?,
    onModeChange: (String) -> Unit,
    onRouteModeChange: (String) -> Unit,
    onProxyShareLanChange: (Boolean) -> Unit,
    onOpenAdvanced: () -> Unit,
    onChooseVpnApps: () -> Unit,
    onReplaceProfile: () -> Unit,
    onCopyRenewalCommand: () -> Unit,
    onCopyPersonalSetupCommand: () -> Unit,
    onOpenOAuthSettings: () -> Unit,
    onConnect: () -> Unit,
    onDisconnect: () -> Unit,
    exitProbeStatus: String,
    exitProbeRunning: Boolean,
    canTestExit: Boolean,
    onTestExit: () -> Unit,
) {
    val uiState = connectionUiState(phase, selected)
    // Metrics belong to the last engine run and can outlive a profile switch.
    // Only a failed connection's own terminal message should trigger recovery.
    val googleGrantExpired = phase == ConnectionPhase.FAILED && isGoogleGrantExpired(message)
    val spokenStatus = when {
        googleGrantExpired -> "Google access needs renewal. Import a renewed profile to reconnect."
        uiState == ConnectionUiState.Error -> connectionActionableMessage(message)?.let {
            "${uiState.title}. $it"
        } ?: uiState.title
        else -> uiState.title
    }
    Panel {
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.spacedBy(12.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Text(
                    text = uiState.title,
                    modifier = Modifier.semantics {
                        contentDescription = spokenStatus
                        heading()
                        liveRegion = LiveRegionMode.Polite
                    },
                    style = MaterialTheme.typography.headlineSmall,
                    fontWeight = FontWeight.SemiBold,
                )
                Text(
                    text = if (running && selected == null) {
                        "Active profile is unavailable. Disconnect and import it again."
                    } else {
                        connectionDetail(
                            selected = selected,
                            selectedMode = selectedMode,
                            proxyShareLan = proxyShareLan,
                        )
                    },
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    style = MaterialTheme.typography.bodyMedium,
                )
                if (uiState == ConnectionUiState.Error && !googleGrantExpired) {
                    connectionActionableMessage(message)?.let { detail ->
                        Text(
                            text = detail,
                            color = MaterialTheme.colorScheme.error,
                            style = MaterialTheme.typography.bodyMedium,
                        )
                    }
                }
            }
        }
        Button(
            onClick = if (running) onDisconnect else if (googleGrantExpired) onReplaceProfile else onConnect,
            enabled = running || (selected != null && (googleGrantExpired || selectedMode != ClientProfile.CONNECTION_MODE_VPN ||
                selected.vpnAppMode != ClientProfile.VPN_APPS_SELECTED || selected.vpnAllowedApps.isNotEmpty())),
            modifier = Modifier.fillMaxWidth(),
            colors = ButtonDefaults.buttonColors(
                containerColor = MaterialTheme.colorScheme.primary,
                contentColor = MaterialTheme.colorScheme.onPrimary,
            ),
        ) {
            Icon(
                if (running) Icons.Rounded.PowerSettingsNew else if (googleGrantExpired) Icons.Rounded.Storage else Icons.Rounded.PlayArrow,
                contentDescription = null,
            )
            Text(if (running) "Disconnect" else if (googleGrantExpired) "Import renewed profile" else "Connect")
        }
        if (phase == ConnectionPhase.CONNECTED) {
            OutlinedButton(
                onClick = onTestExit,
                enabled = !exitProbeRunning && canTestExit,
                modifier = Modifier.fillMaxWidth(),
            ) {
                Icon(Icons.Rounded.Refresh, contentDescription = null)
                Text(if (exitProbeRunning) " Testing exit…" else " Test exit")
            }
            Text(
                exitProbeStatus.ifEmpty { "Local connection is ready. Test the exit on demand to verify a round trip." },
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                style = MaterialTheme.typography.bodySmall,
            )
        }
        GoogleAccessCard(
            hasProfile = selected != null,
            needsRenewal = googleGrantExpired,
            onReplaceProfile = onReplaceProfile,
            onCopyRenewalCommand = onCopyRenewalCommand,
            onCopyPersonalSetupCommand = onCopyPersonalSetupCommand,
            onOpenOAuthSettings = onOpenOAuthSettings,
        )
        HorizontalDivider(color = MaterialTheme.colorScheme.outline)
        SectionHeader(Icons.Rounded.CloudQueue, "Google access route", googleRouteLabel(selected?.routeMode))
        Row(
            modifier = Modifier.selectableGroup(),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            ModeCard(
                icon = Icons.Rounded.CloudQueue,
                title = "Google front",
                subtitle = "www.google.com SNI",
                selected = selected?.routeMode == ClientProfile.ROUTE_GOOGLE_FRONT,
                enabled = selected != null && !running,
                modifier = Modifier.weight(1f),
                onClick = { onRouteModeChange(ClientProfile.ROUTE_GOOGLE_FRONT) },
            )
            ModeCard(
                icon = Icons.Rounded.DataUsage,
                title = "Direct Google",
                subtitle = "Drive API hosts",
                selected = selected?.routeMode == ClientProfile.ROUTE_DIRECT,
                enabled = selected != null && !running,
                modifier = Modifier.weight(1f),
                onClick = { onRouteModeChange(ClientProfile.ROUTE_DIRECT) },
            )
        }
        Text(
            "Choose Google front where direct Google access is unavailable. Reconnect to change routes.",
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            style = MaterialTheme.typography.bodySmall,
        )
        HorizontalDivider(color = MaterialTheme.colorScheme.outline)
        SectionHeader(Icons.Rounded.PowerSettingsNew, "Connection mode", modeLabel(selectedMode))
        ModeSelector(
            selectedMode = selectedMode,
            vpnAppSubtitle = if (selected?.vpnAppMode == ClientProfile.VPN_APPS_SELECTED)
                "${selected?.vpnAllowedApps?.size ?: 0} selected" else "All apps",
            enabled = selected != null && !running,
            onModeChange = onModeChange,
        )
        if (selectedMode == ClientProfile.CONNECTION_MODE_PROXY) {
            SwitchRow(
                title = "Share on LAN",
                detail = proxyAddress(selected, proxyShareLan),
                checked = proxyShareLan,
                enabled = !running,
                onCheckedChange = onProxyShareLanChange,
            )
        } else {
            val selectedApps = selected?.vpnAppMode == ClientProfile.VPN_APPS_SELECTED
            InfoRow(
                Icons.Rounded.VpnKey,
                "VPN mode",
                if (selectedApps) "Routes selected apps through Skirk."
                else "Routes Android app traffic through Skirk.",
            )
            OutlinedButton(
                onClick = onChooseVpnApps,
                enabled = selected != null && !running,
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text(if (selectedApps) "Choose apps · ${selected?.vpnAllowedApps?.size ?: 0} selected" else "Choose apps · All apps")
            }
            Text(
                if (selectedApps) "Only selected apps use Skirk. Other apps use their normal network. Reconnect to change this."
                else "All apps use Skirk except Skirk itself. Select apps to reduce Drive traffic.",
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                style = MaterialTheme.typography.bodySmall,
            )
        }
        HorizontalDivider(color = MaterialTheme.colorScheme.outline)
        DriveUsageSummary(
            metrics = metrics,
            performance = selected?.performance ?: PerformanceSettings.recommended(),
            onOpenAdvanced = onOpenAdvanced,
        )
    }
}

@Composable
private fun GoogleAccessCard(
    hasProfile: Boolean,
    needsRenewal: Boolean,
    onReplaceProfile: () -> Unit,
    onCopyRenewalCommand: () -> Unit,
    onCopyPersonalSetupCommand: () -> Unit,
    onOpenOAuthSettings: () -> Unit,
) {
    var showOwnerGuide by remember { mutableStateOf(false) }
    val guideColor = if (needsRenewal) MaterialTheme.colorScheme.onErrorContainer else MaterialTheme.colorScheme.onSurfaceVariant
    Surface(
        shape = RoundedCornerShape(8.dp),
        color = if (needsRenewal) MaterialTheme.colorScheme.errorContainer else MaterialTheme.colorScheme.surfaceVariant,
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
    ) {
        Column(
            modifier = Modifier.fillMaxWidth().padding(14.dp),
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                Icon(Icons.Rounded.VpnKey, contentDescription = null, modifier = Modifier.size(18.dp))
                Text("Google authorization", fontWeight = FontWeight.SemiBold)
            }
            Text(
                when {
                    needsRenewal -> "Renewal needed"
                    !hasProfile -> "No profile"
                    else -> "Expiry date unknown"
                },
                style = MaterialTheme.typography.titleSmall,
                fontWeight = FontWeight.SemiBold,
            )
            Text(
                when {
                    needsRenewal -> "Google rejected this device's saved access. Ask the exit owner to renew the kit, restart the exit, and send an updated client.skirk."
                    !hasProfile -> "Import a client profile from your exit. The exit owner sets up Google access."
                    else -> "A saved profile cannot show a guaranteed Google refresh-token expiry or the Cloud project's publishing status. Test the exit after connecting to check this session."
                },
                style = MaterialTheme.typography.bodyMedium,
            )
            if (needsRenewal) {
                TextButton(onClick = onCopyRenewalCommand) { Text("Copy command for exit owner") }
                TextButton(onClick = onReplaceProfile) { Text("Import renewed profile") }
            }
            TextButton(onClick = { showOwnerGuide = !showOwnerGuide }) {
                Text(if (showOwnerGuide) "Hide personal OAuth guide" else "Personal OAuth and renewal guide")
            }
            if (showOwnerGuide) {
                Text(
                    "1. For a new self-managed exit, create a Google Cloud project, enable Drive API, create a Desktop OAuth client, and choose Personal OAuth during setup.",
                    color = guideColor,
                    style = MaterialTheme.typography.bodySmall,
                )
                TextButton(onClick = onCopyPersonalSetupCommand) { Text("Copy personal setup command") }
                Text(
                    "2. The project owner opens Google Auth Platform → Audience and adds the signing-in account as a test user while the app is in Testing. Testing refresh tokens expire after seven days. Publishing the app removes that specific limit for new grants; Google may require verification.",
                    color = guideColor,
                    style = MaterialTheme.typography.bodySmall,
                )
                Text(
                    "Only the Google Cloud project owner can publish it; this phone cannot change that setting.",
                    color = guideColor,
                    style = MaterialTheme.typography.bodySmall,
                )
                Text(
                    "3. After publishing or rejected access, run skirk setup renew-oauth --kit skirk-kit on the exit using its actual kit directory. Restart the exit and send an updated client.skirk if this device's grant was rejected. Other revocations remain possible.",
                    color = guideColor,
                    style = MaterialTheme.typography.bodySmall,
                )
                TextButton(onClick = onOpenOAuthSettings) { Text("Open Google Cloud Audience") }
                if (hasProfile && !needsRenewal) {
                    TextButton(onClick = onCopyRenewalCommand) { Text("Copy exit renewal command") }
                }
            }
        }
    }
}

@Composable
private fun ProfileVisibilityButton(visible: Boolean, onToggle: () -> Unit) {
    val action = if (visible) "Hide profile text" else "Show profile text"
    IconButton(
        onClick = onToggle,
        modifier = Modifier.clearAndSetSemantics {
            contentDescription = action
            stateDescription = if (visible) "Visible" else "Hidden"
            role = Role.Button
            onClick(label = action) {
                onToggle()
                true
            }
        },
    ) {
        Icon(
            if (visible) Icons.Rounded.VisibilityOff else Icons.Rounded.Visibility,
            contentDescription = null,
        )
    }
}

@Composable
private fun ReplaceProfileDialog(
    profileName: String,
    rawConfig: String,
    error: String,
    onRawConfigChange: (String) -> Unit,
    onPaste: () -> Unit,
    onChooseFile: () -> Unit,
    readingFile: Boolean,
    onCancelFile: () -> Unit,
    onReplace: () -> Unit,
    onDismiss: () -> Unit,
) {
    var profileTextVisible by remember { mutableStateOf(false) }
    AlertDialog(
        onDismissRequest = onDismiss,
        containerColor = MaterialTheme.colorScheme.surface,
        title = { Text("Replace $profileName") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                Text(
                    "Paste or choose the renewed client.skirk. This keeps your Google route, local ports, " +
                        "connection mode, app selection, and speed settings.",
                    style = MaterialTheme.typography.bodyMedium,
                )
                OutlinedTextField(
                    value = rawConfig,
                    onValueChange = onRawConfigChange,
                    modifier = Modifier.fillMaxWidth(),
                    minLines = 4,
                    maxLines = 6,
                    label = { Text("Renewed client.skirk") },
                    isError = error.isNotBlank(),
                    supportingText = if (error.isNotBlank()) ({ Text(error) }) else null,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                    visualTransformation = if (profileTextVisible) VisualTransformation.None else PasswordVisualTransformation(),
                    trailingIcon = {
                        ProfileVisibilityButton(profileTextVisible) { profileTextVisible = !profileTextVisible }
                    },
                )
                if (readingFile) {
                    Text("Reading profile file…", modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite })
                    TextButton(onClick = onCancelFile) { Text("Cancel file import") }
                }
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedButton(onClick = {
                        profileTextVisible = false
                        onPaste()
                    }) {
                        Icon(Icons.Rounded.ContentPaste, contentDescription = null)
                        Text("Paste")
                    }
                    OutlinedButton(onClick = {
                        profileTextVisible = false
                        onChooseFile()
                    }) {
                        Icon(Icons.Rounded.Storage, contentDescription = null)
                        Text("Choose file")
                    }
                }
            }
        },
        confirmButton = {
            TextButton(onClick = onReplace, enabled = !readingFile && rawConfig.isNotBlank()) {
                Text("Replace")
            }
        },
        dismissButton = {
            TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

@Composable
private fun AdvancedSettingsDialog(
    performance: PerformanceSettings,
    metrics: DriveMetrics?,
    enabled: Boolean,
    onDismiss: () -> Unit,
    onPerformanceChange: (PerformanceSettings) -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        confirmButton = {
            TextButton(onClick = onDismiss) {
                Text("Done")
            }
        },
        icon = { Icon(Icons.Rounded.Tune, contentDescription = null) },
        title = { Text("Advanced settings") },
        text = {
            Column(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(max = 520.dp)
                    .verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                DriveUsageDetails(metrics = metrics)
                HorizontalDivider(color = MaterialTheme.colorScheme.outline)
                PerformanceSettingsPanel(
                    performance = performance,
                    enabled = enabled,
                    onPerformanceChange = onPerformanceChange,
                )
            }
        },
    )
}

private data class VpnAppChoice(val packageName: String, val label: String)

@Suppress("DEPRECATION")
private fun installedVpnApps(context: Context): List<VpnAppChoice> {
    val manager = context.packageManager
    val launcher = Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER)
    return manager.queryIntentActivities(launcher, 0)
        .mapNotNull { activity ->
            val appPackage = activity.activityInfo?.packageName ?: return@mapNotNull null
            if (appPackage == context.packageName) return@mapNotNull null
            VpnAppChoice(appPackage, activity.loadLabel(manager).toString().ifBlank { appPackage })
        }
        .distinctBy { it.packageName }
        .sortedWith(compareBy<VpnAppChoice> { it.label.lowercase() }.thenBy { it.packageName })
}

@Composable
private fun VpnAppsDialog(
    profile: ClientProfile,
    onDismiss: () -> Unit,
    onSave: (String, List<String>) -> Unit,
) {
    val context = LocalContext.current
    val installed = remember(context) { installedVpnApps(context) }
    var selectedOnly by remember(profile.id) { mutableStateOf(profile.vpnAppMode == ClientProfile.VPN_APPS_SELECTED) }
    var selectedPackages by remember(profile.id) { mutableStateOf(profile.vpnAllowedApps.toSet()) }
    var search by remember { mutableStateOf("") }
    val installedNames = installed.map { it.packageName }.toSet()
    val unavailable = selectedPackages.filterNot { it in installedNames || it == context.packageName }
        .map { VpnAppChoice(it, it) }
    val visible = (installed + unavailable).filter {
        search.isBlank() || it.label.contains(search, ignoreCase = true) ||
            it.packageName.contains(search, ignoreCase = true)
    }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Choose VPN apps") },
        text = {
            Column(
                modifier = Modifier.fillMaxWidth().heightIn(max = 520.dp),
                verticalArrangement = Arrangement.spacedBy(10.dp),
            ) {
                SwitchRow(
                    title = "Only selected apps",
                    detail = "Off routes all apps through Skirk (except Skirk itself).",
                    checked = selectedOnly,
                    enabled = true,
                    onCheckedChange = { selectedOnly = it },
                )
                if (selectedOnly) {
                    Text(
                        "Unselected apps use their normal network. Android may block them if ‘Block connections without VPN’ is enabled. Changes apply after reconnecting.",
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        style = MaterialTheme.typography.bodySmall,
                    )
                    OutlinedTextField(
                        value = search,
                        onValueChange = { search = it },
                        label = { Text("Search apps") },
                        singleLine = true,
                        modifier = Modifier.fillMaxWidth(),
                    )
                    LazyColumn(
                        modifier = Modifier.fillMaxWidth().heightIn(max = 320.dp),
                        verticalArrangement = Arrangement.spacedBy(6.dp),
                    ) {
                        items(visible.size, key = { visible[it].packageName }) { index ->
                            val app = visible[index]
                            SwitchRow(
                                title = app.label,
                                detail = if (app.packageName in installedNames) app.packageName
                                    else "Unavailable · ${app.packageName}",
                                checked = app.packageName in selectedPackages,
                                enabled = true,
                                onCheckedChange = { checked ->
                                    selectedPackages = if (checked) selectedPackages + app.packageName
                                        else selectedPackages - app.packageName
                                },
                            )
                        }
                    }
                    if (installed.isEmpty()) {
                        Text("No launchable apps were found on this device.")
                    }
                    Text(
                        "${selectedPackages.size} selected",
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        style = MaterialTheme.typography.bodySmall,
                    )
                }
            }
        },
        confirmButton = {
            TextButton(
                enabled = !selectedOnly || selectedPackages.any { it in installedNames },
                onClick = {
                    onSave(
                        if (selectedOnly) ClientProfile.VPN_APPS_SELECTED else ClientProfile.VPN_APPS_ALL,
                        selectedPackages.filterNot { it == context.packageName }.sorted(),
                    )
                },
            ) { Text("Save") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

@Composable
private fun PerformanceSettingsPanel(
    performance: PerformanceSettings,
    enabled: Boolean,
    onPerformanceChange: (PerformanceSettings) -> Unit,
) {
    SectionHeader(Icons.Rounded.Speed, "Performance", performancePresetLabel(performance.preset))
    listOf(
        listOf(PerformanceSettings.PRESET_LOWER_USAGE, PerformanceSettings.PRESET_RECOMMENDED),
        listOf(PerformanceSettings.PRESET_RESPONSIVE, PerformanceSettings.PRESET_BULK_TRANSFER),
    ).forEach { row ->
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.fillMaxWidth()) {
            row.forEach { preset ->
                ModeCard(
                    icon = Icons.Rounded.Speed,
                    title = performancePresetLabel(preset),
                    subtitle = performancePresetDetail(PerformanceSettings.forPreset(preset)),
                    selected = performance.preset == preset,
                    enabled = enabled,
                    modifier = Modifier.weight(1f),
                    onClick = { onPerformanceChange(PerformanceSettings.forPreset(preset)) },
                )
            }
        }
    }
    OutlinedButton(
        onClick = { onPerformanceChange(performance.copy(preset = PerformanceSettings.PRESET_CUSTOM)) },
        enabled = enabled,
        modifier = Modifier.fillMaxWidth(),
    ) {
        Icon(Icons.Rounded.Settings, contentDescription = null)
        Text("Custom keeps current values")
    }
    if (performance.preset == PerformanceSettings.PRESET_CUSTOM) {
        AdjustValueRow(
            title = "Check interval",
            detail = "Higher values use fewer Drive list calls.",
            valueText = "${performance.pollMs} ms",
            enabled = enabled,
            onDecrease = {
                onPerformanceChange(performance.copy(pollMs = (performance.pollMs - 250).coerceAtLeast(PerformanceSettings.CUSTOM_MIN_POLL_MS)))
            },
            onIncrease = {
                onPerformanceChange(performance.copy(pollMs = (performance.pollMs + 250).coerceAtMost(60000)))
            },
        )
        AdjustValueRow(
            title = "Upload workers",
            detail = "More workers can open streams faster but spend quota sooner.",
            valueText = performance.uploadConcurrency.toString(),
            enabled = enabled,
            onDecrease = {
                onPerformanceChange(performance.copy(uploadConcurrency = (performance.uploadConcurrency - 2).coerceAtLeast(1)))
            },
            onIncrease = {
                onPerformanceChange(performance.copy(uploadConcurrency = (performance.uploadConcurrency + 2).coerceAtMost(PerformanceSettings.CUSTOM_MAX_UPLOAD_WORKERS)))
            },
        )
        AdjustValueRow(
            title = "Download workers",
            detail = "More workers help bulk downloads and can crowd interactive traffic.",
            valueText = performance.downloadConcurrency.toString(),
            enabled = enabled,
            onDecrease = {
                onPerformanceChange(performance.copy(downloadConcurrency = (performance.downloadConcurrency - 2).coerceAtLeast(1)))
            },
            onIncrease = {
                onPerformanceChange(performance.copy(downloadConcurrency = (performance.downloadConcurrency + 2).coerceAtMost(PerformanceSettings.CUSTOM_MAX_DOWNLOAD_WORKERS)))
            },
        )
        SwitchRow(
            title = "Burst polling",
            detail = "Faster short window after traffic; useful but costly.",
            checked = performance.burstPoll,
            enabled = enabled,
            onCheckedChange = { onPerformanceChange(performance.copy(burstPoll = it)) },
        )
    }
    InfoRow(
        Icons.Rounded.Tune,
        "Current limits",
        "${performance.pollMs}ms check · ${performance.uploadConcurrency} up · ${performance.downloadConcurrency} down · ${if (performance.burstPoll) "burst on" else "burst off"}",
    )
    if (performance.burstPoll) {
        InfoRow(
            Icons.Rounded.CloudQueue,
            "Drive API warning",
            "Burst polling checks faster after traffic and can burn list quota quickly.",
        )
    }
    if (performance.preset == PerformanceSettings.PRESET_CUSTOM) {
        InfoRow(
            Icons.Rounded.Tune,
            "Custom warning",
            "Low check intervals and high workers can burn Drive quota quickly.",
        )
    }
}

@Composable
private fun AdjustValueRow(
    title: String,
    detail: String,
    valueText: String,
    enabled: Boolean,
    onDecrease: () -> Unit,
    onIncrease: () -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(8.dp))
            .padding(12.dp),
        horizontalArrangement = Arrangement.spacedBy(10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(title, fontWeight = FontWeight.Medium)
            Text(detail, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        OutlinedButton(onClick = onDecrease, enabled = enabled) {
            Text("-")
        }
        Text(valueText, fontWeight = FontWeight.SemiBold)
        OutlinedButton(onClick = onIncrease, enabled = enabled) {
            Text("+")
        }
    }
}

@Composable
private fun DriveUsageSummary(
    metrics: DriveMetrics?,
    performance: PerformanceSettings,
    onOpenAdvanced: () -> Unit,
) {
    val displayUnits = metrics?.unitsPerMinute ?: 0.0
    val errors = metrics?.errorsPerMinute ?: 0.0
    val label = drivePressureLabel(displayUnits, errors, metrics?.lastErrorReason.orEmpty())
    val percent = drivePressurePercent(displayUnits)
    val percentLabel = if (displayUnits > 0.0 && percent == 0) "<1%" else "$percent%"
    Surface(
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
        color = MaterialTheme.colorScheme.surfaceVariant,
    ) {
        Column(
            modifier = Modifier.padding(12.dp),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Row(
                    modifier = Modifier.weight(1f),
                    horizontalArrangement = Arrangement.spacedBy(8.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Icon(Icons.Rounded.DataUsage, contentDescription = null, modifier = Modifier.size(18.dp))
                    Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
                        Text("Client Drive activity", fontWeight = FontWeight.Medium)
                        Text(
                            if (metrics == null) "No live measurements yet." else "Measured on this phone only.",
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            style = MaterialTheme.typography.bodySmall,
                        )
                    }
                }
                TextButton(onClick = onOpenAdvanced) {
                    Icon(Icons.Rounded.Settings, contentDescription = null)
                    Text("Advanced", maxLines = 1)
                }
            }
            if (metrics != null) {
                QuotaBar(unitsPerMinute = displayUnits, errorsPerMinute = errors)
                Row(
                    modifier = Modifier.fillMaxWidth(),
                    horizontalArrangement = Arrangement.SpaceBetween,
                ) {
                    Text("$label · $percentLabel", fontWeight = FontWeight.SemiBold)
                    Text(
                        "~${displayUnits.toInt()} units/min",
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        style = MaterialTheme.typography.bodyMedium,
                    )
                }
            } else {
                Text(
                    "Idle client list estimate · 0 units/min with no active streams",
                    fontWeight = FontWeight.SemiBold,
                )
            }
            Text(
                "Exit and shared project usage are excluded.",
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                style = MaterialTheme.typography.bodySmall,
            )
            Text(
                "Performance: ${performancePresetLabel(performance.preset)} · ${performancePresetDetail(performance)}",
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                style = MaterialTheme.typography.bodySmall,
            )
            if (metrics?.backoffActive == true) {
                Text(
                    "Drive cooldown: ${metrics.backoffReason.ifBlank { "rate limited" }} · ${metrics.backoffWaitSeconds.toInt()}s",
                    color = MaterialTheme.colorScheme.error,
                    style = MaterialTheme.typography.bodySmall,
                )
            }
        }
    }
}

@Composable
private fun DriveUsageDetails(metrics: DriveMetrics?) {
    val measuredUnits = metrics?.unitsPerMinute
    Column(verticalArrangement = Arrangement.spacedBy(10.dp)) {
        SectionHeader(
            Icons.Rounded.CloudQueue,
            "Drive API usage",
            measuredUnits?.let { drivePressureLabel(it, metrics.errorsPerMinute, metrics.lastErrorReason) } ?: "Client estimate",
        )
        if (measuredUnits != null) {
            QuotaBar(unitsPerMinute = measuredUnits, errorsPerMinute = metrics.errorsPerMinute)
        }
        InfoRow(
            Icons.Rounded.DataUsage,
            "Current phone",
            measuredUnits?.let { "${it.toInt()} units/min · ${metrics.ops.ifBlank { "no ops yet" }}" }
                ?: "Connect to measure this phone's local Drive API usage.",
        )
        InfoRow(
            Icons.Rounded.Speed,
            "Idle client list estimate",
            "0 units/min with no active streams. A local request wakes Drive polling immediately. " +
                "The exit and shared project are not included.",
        )
        InfoRow(
            Icons.Rounded.CloudQueue,
            "What this means",
            "Skirk uses Google Drive requests, not Drive storage. The exit and other clients share the same Google budget.",
        )
        if (metrics?.backoffActive == true) {
            InfoRow(
                Icons.Rounded.CloudQueue,
                "Drive cooldown",
                "${metrics.backoffReason.ifBlank { "rate limited" }} · ${metrics.backoffWaitSeconds.toInt()}s remaining",
            )
        }
    }
}

@Composable
private fun QuotaBar(unitsPerMinute: Double, errorsPerMinute: Double) {
    val pressure = drivePressureFraction(unitsPerMinute)
    val color = when {
        errorsPerMinute > 0.0 -> Color(0xFFDC2626)
        pressure >= 0.7f -> Color(0xFFEA580C)
        pressure >= 0.3f -> Color(0xFFEAB308)
        else -> Color(0xFF22C55E)
    }
    Box(
        modifier = Modifier
            .fillMaxWidth()
            .height(9.dp)
            .background(MaterialTheme.colorScheme.outline, RoundedCornerShape(999.dp)),
    ) {
        Box(
            modifier = Modifier
                .fillMaxWidth(pressure.coerceIn(0.03f, 1f))
                .height(9.dp)
                .background(color, RoundedCornerShape(999.dp)),
        )
    }
}

@Composable
private fun DiagnosticsPanel(
    logText: String,
    expanded: Boolean,
    onToggleExpanded: () -> Unit,
    onRefresh: () -> Unit,
    onCopy: () -> Unit,
) {
    Panel {
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Rounded.Storage, contentDescription = null, modifier = Modifier.size(18.dp))
                    Text("Diagnostics", fontWeight = FontWeight.SemiBold)
                }
                Text(
                    if (expanded) "Sidecar logs and support capture" else "Logs are collapsed until you need them.",
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    style = MaterialTheme.typography.bodyMedium,
                )
            }
            TextButton(onClick = onToggleExpanded) {
                Text(if (expanded) "Hide" else "Show")
            }
        }
        if (expanded) {
            Surface(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(min = 120.dp, max = 260.dp),
                shape = RoundedCornerShape(8.dp),
                color = MaterialTheme.colorScheme.surfaceVariant,
                border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
            ) {
                Text(
                    text = logText.ifBlank { "No logs yet." },
                    modifier = Modifier
                        .verticalScroll(rememberScrollState())
                        .padding(12.dp),
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    style = MaterialTheme.typography.bodySmall,
                )
            }
            Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                OutlinedButton(onClick = onRefresh) {
                    Icon(Icons.Rounded.Refresh, contentDescription = null)
                    Text("Refresh")
                }
                OutlinedButton(onClick = onCopy, enabled = logText.isNotBlank()) {
                    Icon(Icons.Rounded.ContentCopy, contentDescription = null)
                    Text("Copy")
                }
            }
        } else {
            InfoRow(
                Icons.Rounded.Storage,
                "Diagnostics are secondary",
                "Open this section for startup failures, tunnel output, or support capture.",
            )
        }
    }
}

@Composable
private fun AddProfilePanel(onExpand: () -> Unit) {
    Panel {
        Row(
            modifier = Modifier.fillMaxWidth(),
            horizontalArrangement = Arrangement.SpaceBetween,
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Row(
                modifier = Modifier.weight(1f),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Icon(Icons.Rounded.Add, contentDescription = null, modifier = Modifier.size(18.dp))
                Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
                    Text("Add profile", fontWeight = FontWeight.SemiBold)
                    Text(
                        "Import another config",
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        style = MaterialTheme.typography.bodyMedium,
                    )
                }
            }
            OutlinedButton(onClick = onExpand) {
                Icon(Icons.Rounded.Add, contentDescription = null)
                Text("Add")
            }
        }
    }
}

@Composable
private fun ImportPanel(
    enabled: Boolean,
    profileName: String,
    socksPort: String,
    httpPort: String,
    rawConfig: String,
    importError: String,
    onProfileNameChange: (String) -> Unit,
    onSocksPortChange: (String) -> Unit,
    onHttpPortChange: (String) -> Unit,
    onRawConfigChange: (String) -> Unit,
    onPaste: () -> Unit,
    onChooseFile: () -> Unit,
    readingFile: Boolean,
    onCancelFile: () -> Unit,
    onImport: () -> Unit,
) {
    var profileTextVisible by remember { mutableStateOf(false) }
    var showPasteInput by remember { mutableStateOf(false) }
    var showLocalPorts by remember { mutableStateOf(socksPort != "18080" || httpPort != "18081") }
    Panel {
        SectionHeader(Icons.Rounded.Add, "Import profile", "Start with the file from your exit")
        Button(onClick = {
            profileTextVisible = false
            showPasteInput = false
            onChooseFile()
        }, enabled = enabled, modifier = Modifier.fillMaxWidth()) {
            Text("Choose client.skirk file")
        }
        if (readingFile) {
            Text("Reading profile file…", modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite })
            TextButton(onClick = onCancelFile) { Text("Cancel file import") }
        }
        if (!showPasteInput) {
            TextButton(onClick = { showPasteInput = true }, enabled = enabled) {
                Text("Paste a profile instead")
            }
        } else {
            OutlinedTextField(
                value = rawConfig,
                onValueChange = onRawConfigChange,
                modifier = Modifier.fillMaxWidth(),
                minLines = 3,
                label = { Text("Skirk profile or client.json") },
                isError = importError.isNotBlank(),
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password),
                visualTransformation = if (profileTextVisible) VisualTransformation.None else PasswordVisualTransformation(),
                trailingIcon = {
                    ProfileVisibilityButton(profileTextVisible) { profileTextVisible = !profileTextVisible }
                },
                supportingText = {
                    if (importError.isNotBlank()) Text(importError)
                    else Text("Paste a one-line profile or client.json.")
                },
            )
            OutlinedButton(onClick = {
                profileTextVisible = false
                onPaste()
            }, enabled = enabled) {
                Icon(Icons.Rounded.ContentPaste, contentDescription = null)
                Text("Paste from clipboard")
            }
        }
        if (rawConfig.isNotBlank() || importError.isNotBlank()) {
            if (!showPasteInput && rawConfig.isNotBlank()) {
                Text("Profile ready to import", color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            if (!showPasteInput && importError.isNotBlank()) {
                Text(importError, color = MaterialTheme.colorScheme.error)
            }
            OutlinedTextField(
                value = profileName,
                onValueChange = onProfileNameChange,
                modifier = Modifier.fillMaxWidth(),
                label = { Text("Profile name") },
                singleLine = true,
            )
            Button(
                onClick = onImport,
                enabled = enabled && !readingFile && rawConfig.isNotBlank(),
                modifier = Modifier.fillMaxWidth(),
                colors = ButtonDefaults.buttonColors(
                    containerColor = MaterialTheme.colorScheme.primary,
                    contentColor = MaterialTheme.colorScheme.onPrimary,
                ),
            ) {
                Icon(Icons.Rounded.Add, contentDescription = null)
                Text("Import profile")
            }
            TextButton(onClick = { showLocalPorts = !showLocalPorts }) {
                Text(if (showLocalPorts) "Hide local ports" else "Change local ports")
            }
            if (showLocalPorts) {
                OutlinedTextField(
                    value = socksPort,
                    onValueChange = onSocksPortChange,
                    modifier = Modifier.fillMaxWidth(),
                    label = { Text("Local SOCKS port") },
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                )
                OutlinedTextField(
                    value = httpPort,
                    onValueChange = onHttpPortChange,
                    modifier = Modifier.fillMaxWidth(),
                    label = { Text("Local HTTP proxy port") },
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                )
            }
        }
    }
}

@Composable
private fun ProfilesPanel(
    profiles: List<ClientProfile>,
    selectedId: String?,
    running: Boolean,
    onSelect: (ClientProfile) -> Unit,
    onDelete: (ClientProfile) -> Unit,
    onReplace: (ClientProfile) -> Unit,
) {
    Panel {
        SectionHeader(Icons.Rounded.AccountCircle, "Profiles", "${profiles.size} saved")
        if (profiles.isEmpty()) {
            EmptyState()
        } else {
            profiles.forEach { profile ->
                ProfileRow(
                    profile = profile,
                    selected = profile.id == selectedId,
                    enabled = !running,
                    onSelect = { onSelect(profile) },
                    onDelete = { onDelete(profile) },
                )
            }
            profiles.firstOrNull { it.id == selectedId }?.let { selected ->
                OutlinedButton(
                    onClick = { onReplace(selected) },
                    enabled = !running,
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Icon(Icons.Rounded.Refresh, contentDescription = null)
                    Text("Replace selected config")
                }
                Text(
                    "Use after renewing Google access. Your route, ports, and VPN app choices stay saved.",
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    style = MaterialTheme.typography.bodySmall,
                )
            }
        }
    }
}

@Composable
private fun Panel(content: @Composable ColumnScope.() -> Unit) {
    Surface(
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
        color = MaterialTheme.colorScheme.surface,
        tonalElevation = 0.dp,
    ) {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .padding(16.dp),
            verticalArrangement = Arrangement.spacedBy(12.dp),
            content = content,
        )
    }
}

@Composable
private fun SectionHeader(icon: ImageVector, title: String, detail: String, modifier: Modifier = Modifier) {
    Row(
        modifier = modifier.fillMaxWidth(),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
        verticalAlignment = Alignment.Top,
    ) {
        Icon(icon, contentDescription = null, modifier = Modifier.size(18.dp))
        Column(modifier = Modifier.weight(1f)) {
            Text(title, fontWeight = FontWeight.SemiBold)
            Text(
                detail,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                style = MaterialTheme.typography.bodySmall,
            )
        }
    }
}

@Composable
private fun ModeSelector(
    selectedMode: String,
    vpnAppSubtitle: String,
    enabled: Boolean,
    onModeChange: (String) -> Unit,
) {
    Row(
        modifier = Modifier.selectableGroup(),
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        ModeCard(
            icon = Icons.Rounded.VpnKey,
            title = "VPN",
            subtitle = vpnAppSubtitle,
            selected = selectedMode == ClientProfile.CONNECTION_MODE_VPN,
            enabled = enabled,
            modifier = Modifier.weight(1f),
            onClick = { onModeChange(ClientProfile.CONNECTION_MODE_VPN) },
        )
        ModeCard(
            icon = Icons.Rounded.WifiTethering,
            title = "Proxy",
            subtitle = "SOCKS5 + HTTP",
            selected = selectedMode == ClientProfile.CONNECTION_MODE_PROXY,
            enabled = enabled,
            modifier = Modifier.weight(1f),
            onClick = { onModeChange(ClientProfile.CONNECTION_MODE_PROXY) },
        )
    }
}

@Composable
private fun ModeCard(
    icon: ImageVector,
    title: String,
    subtitle: String,
    selected: Boolean,
    enabled: Boolean,
    modifier: Modifier = Modifier,
    onClick: () -> Unit,
) {
    Surface(
        modifier = modifier.selectable(
            selected = selected,
            enabled = enabled,
            role = Role.RadioButton,
            onClick = onClick,
        ),
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(
            1.dp,
            if (selected) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.outline,
        ),
        color = if (selected) MaterialTheme.colorScheme.surfaceVariant else MaterialTheme.colorScheme.surface,
        contentColor = MaterialTheme.colorScheme.onSurface,
    ) {
        Column(
            modifier = Modifier.padding(14.dp),
            verticalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween,
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Icon(icon, contentDescription = null, modifier = Modifier.size(18.dp))
                if (selected) {
                    Icon(Icons.Rounded.Check, contentDescription = null, modifier = Modifier.size(18.dp))
                }
            }
            Text(title, fontWeight = FontWeight.SemiBold)
            Text(
                subtitle,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                style = MaterialTheme.typography.bodySmall,
            )
        }
    }
}

@Composable
private fun SwitchRow(
    title: String,
    detail: String,
    checked: Boolean,
    enabled: Boolean,
    onCheckedChange: (Boolean) -> Unit,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(8.dp))
            .padding(12.dp),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(title, fontWeight = FontWeight.Medium)
            Text(detail, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(
            checked = checked,
            enabled = enabled,
            onCheckedChange = onCheckedChange,
            colors = SwitchDefaults.colors(
                checkedTrackColor = MaterialTheme.colorScheme.primary,
                checkedThumbColor = MaterialTheme.colorScheme.onPrimary,
            ),
        )
    }
}

@Composable
private fun InfoRow(icon: ImageVector, title: String, detail: String) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(8.dp))
            .padding(12.dp),
        horizontalArrangement = Arrangement.spacedBy(10.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(icon, contentDescription = null, modifier = Modifier.size(18.dp))
        Column(verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(title, fontWeight = FontWeight.Medium)
            Text(detail, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
private fun ProfileRow(
    profile: ClientProfile,
    selected: Boolean,
    enabled: Boolean,
    onSelect: () -> Unit,
    onDelete: () -> Unit,
) {
    Surface(
        modifier = Modifier.semantics {
            stateDescription = if (selected) "Selected profile" else "Not selected"
        },
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(
            1.dp,
            if (selected) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.outline,
        ),
        color = if (selected) MaterialTheme.colorScheme.surfaceVariant else MaterialTheme.colorScheme.surface,
        contentColor = MaterialTheme.colorScheme.onSurface,
    ) {
        Row(
            modifier = Modifier
                .fillMaxWidth()
                .padding(12.dp),
            horizontalArrangement = Arrangement.spacedBy(10.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                    if (selected) {
                        Icon(Icons.Rounded.Check, contentDescription = null, modifier = Modifier.size(16.dp))
                    } else {
                        Icon(Icons.Rounded.AccountCircle, contentDescription = null, modifier = Modifier.size(16.dp))
                    }
                    Text(profile.name, fontWeight = FontWeight.SemiBold)
                }
                Text(
                    "${googleRouteLabel(profile.routeMode)} / SOCKS ${profile.socksAddress} / HTTP ${profile.httpAddress}",
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
            if (!selected) {
                OutlinedButton(onClick = onSelect, enabled = enabled) {
                    Text("Select")
                }
            }
            IconButton(onClick = onDelete, enabled = enabled) {
                Icon(Icons.Rounded.Delete, contentDescription = "Delete ${profile.name}")
            }
        }
    }
}

@Composable
private fun EmptyState() {
    Surface(
        modifier = Modifier.fillMaxWidth(),
        shape = RoundedCornerShape(8.dp),
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outline),
        color = MaterialTheme.colorScheme.surfaceVariant,
    ) {
        Column(
            modifier = Modifier.padding(18.dp),
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            Icon(Icons.Rounded.Storage, contentDescription = null)
            Text("No profiles yet", color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

private enum class ConnectionUiState(val title: String) {
    Setup("Setup"),
    Ready("Ready"),
    Connecting("Connecting"),
    Connected("Connected"),
    Error("Needs attention"),
}

private fun connectionUiState(phase: ConnectionPhase, selected: ClientProfile?): ConnectionUiState = when {
    phase == ConnectionPhase.CONNECTING -> ConnectionUiState.Connecting
    phase == ConnectionPhase.CONNECTED -> ConnectionUiState.Connected
    selected == null -> ConnectionUiState.Setup
    phase == ConnectionPhase.FAILED -> ConnectionUiState.Error
    else -> ConnectionUiState.Ready
}

private fun connectionDetail(selected: ClientProfile?, selectedMode: String, proxyShareLan: Boolean): String {
    if (selected == null) {
        return "Import a profile to enable connection."
    }
    return "${selected.name} · ${googleRouteLabel(selected.routeMode)} · " +
        connectionRouteDetail(selected, selectedMode, proxyShareLan)
}

private fun connectionActionableMessage(message: String): String? {
    val normalized = message.trim()
    if (normalized.isBlank()) {
        return null
    }
    if (isGoogleGrantExpired(normalized)) {
        return "Google rejected this device's saved access. See recovery steps below."
    }
    val lower = normalized.lowercase()
    if (
        lower == "disconnected" ||
        lower == "vpn connected" ||
        lower.startsWith("socks connected") ||
        lower.startsWith("proxy connected") ||
        lower.endsWith("stopped") ||
        lower.startsWith("imported ")
    ) {
        return null
    }
    return normalized
}

private fun isGoogleGrantExpired(message: String): Boolean {
    val compact = message.lowercase().filter(Char::isLetterOrDigit)
    return "invalidgrant" in compact ||
        "googleauthorizationexpired" in compact ||
        "refreshtokenexpired" in compact
}

private fun modeLabel(mode: String): String =
    if (mode == ClientProfile.CONNECTION_MODE_PROXY) "Proxy" else "VPN"

private fun googleRouteLabel(mode: String?): String = when (mode) {
    ClientProfile.ROUTE_GOOGLE_FRONT -> "Google front"
    ClientProfile.ROUTE_DIRECT -> "Direct Google"
    null -> "Not configured"
    else -> "Custom route ($mode)"
}

private fun connectionRouteDetail(
    selected: ClientProfile,
    selectedMode: String,
    proxyShareLan: Boolean,
): String = when {
    selectedMode == ClientProfile.CONNECTION_MODE_PROXY && proxyShareLan ->
        "LAN proxy · ${proxyEndpoints(selected, true)}"
    selectedMode == ClientProfile.CONNECTION_MODE_PROXY ->
        "Local proxy · ${proxyEndpoints(selected, false)}"
    selected.vpnAppMode == ClientProfile.VPN_APPS_SELECTED -> {
        val count = selected.vpnAllowedApps.size
        "VPN for $count selected ${if (count == 1) "app" else "apps"}"
    }
    else -> "VPN for all apps"
}

private fun proxyAddress(profile: ClientProfile?, shareLan: Boolean): String {
    if (profile == null) {
        return "Import or select a profile first."
    }
    if (!shareLan) {
        return "This phone only: ${proxyEndpoints(profile, false)}"
    }
    return "Use only on trusted networks: ${proxyEndpoints(profile, true)}"
}

private fun proxyEndpoints(profile: ClientProfile, shareLan: Boolean): String {
    if (!shareLan) {
        return "SOCKS5 ${profile.socksAddress} · HTTP ${profile.httpAddress}"
    }
    val socksAddress = AndroidSkirkEngine.lanAddresses(profile.socksPort)
        .firstOrNull()
        ?: "0.0.0.0:${profile.socksPort}"
    val httpAddress = AndroidSkirkEngine.lanAddresses(profile.httpPort)
        .firstOrNull()
        ?: "0.0.0.0:${profile.httpPort}"
    return "SOCKS5 $socksAddress · HTTP $httpAddress"
}

@Suppress("DEPRECATION")
private fun isSkirkServiceRunning(context: Context, mode: String): Boolean {
    val manager = context.getSystemService(ActivityManager::class.java) ?: return true
    val expectedClass = when (mode) {
        ClientProfile.CONNECTION_MODE_PROXY -> SkirkProxyService::class.java.name
        ClientProfile.CONNECTION_MODE_VPN -> SkirkVpnService::class.java.name
        else -> return false
    }
    return manager.getRunningServices(Int.MAX_VALUE)
        .any { service -> service.service.className == expectedClass }
}

private fun readSkirkLogs(context: Context, activeMode: String): String {
    val logsDir = File(context.filesDir, "logs")
    if (!logsDir.exists()) {
        return ""
    }
    val files = logsDir.listFiles()
        ?.filter { it.isFile && it.name.endsWith(".log") }
        .orEmpty()
    val ordered = when (activeMode) {
        ClientProfile.CONNECTION_MODE_PROXY -> files.filter { it.name == "skirk-client.log" }
        ClientProfile.CONNECTION_MODE_VPN -> files.filter { it.name == "skirk-vpn-client.log" }
        else -> files.sortedBy { it.name }
    }
    return ordered
        .joinToString("\n\n") { file ->
            val text = file.readTail(maxBytes = 64 * 1024, maxLines = 240)
            "== ${file.name} ==\n$text"
        }
        .takeLast(96 * 1024)
}

private data class DriveMetrics(
    val unitsPerMinute: Double,
    val errorsPerMinute: Double,
    val lastErrorReason: String,
    val ops: String,
    val backoffActive: Boolean,
    val backoffReason: String,
    val backoffWaitSeconds: Double,
)

private fun readDriveMetrics(context: Context, activeMode: String): DriveMetrics? {
    val logsDir = File(context.filesDir, "logs")
    val fileName = when (activeMode) {
        ClientProfile.CONNECTION_MODE_PROXY -> "skirk-client.metrics.json"
        ClientProfile.CONNECTION_MODE_VPN -> "skirk-vpn-client.metrics.json"
        else -> return null
    }
    val file = File(logsDir, fileName)
    if (!file.exists() || file.length() == 0L) {
        return null
    }
    return runCatching {
        val json = JSONObject(file.readText())
        val rate = json.optJSONObject("recent_quota_per_minute")
            ?: json.optJSONObject("recentQuotaPerMinute")
            ?: JSONObject()
        val backoff = json.optJSONObject("drive_backoff")
            ?: json.optJSONObject("driveBackoff")
            ?: JSONObject()
        DriveMetrics(
            unitsPerMinute = rate.optDouble("units", 0.0),
            errorsPerMinute = rate.optDouble("errors", 0.0),
            lastErrorReason = rate.optString("last_error_reason", rate.optString("lastErrorReason", "")),
            ops = json.optString(
                "recent_quota_ops",
                json.optString("recentQuotaOps", ""),
            ),
            backoffActive = backoff.optBoolean("active", false),
            backoffReason = backoff.optString("reason", ""),
            backoffWaitSeconds = backoff.optDouble("wait_seconds", backoff.optDouble("waitSeconds", 0.0)),
        )
    }.getOrNull()
}

private fun drivePressureFraction(unitsPerMinute: Double): Float =
    (unitsPerMinute / DRIVE_USER_UNITS_PER_MINUTE).coerceIn(0.0, 1.0).toFloat()

private fun drivePressurePercent(unitsPerMinute: Double): Int =
    (drivePressureFraction(unitsPerMinute) * 100).toInt()

private fun drivePressureLabel(unitsPerMinute: Double, errorsPerMinute: Double): String = when {
    errorsPerMinute > 0.0 -> driveErrorReasonLabel("")
    unitsPerMinute <= 0.0 -> "Not measured"
    unitsPerMinute < DRIVE_USER_UNITS_PER_MINUTE * 0.08 -> "Normal"
    unitsPerMinute < DRIVE_USER_UNITS_PER_MINUTE * 0.30 -> "Moderate"
    unitsPerMinute < DRIVE_USER_UNITS_PER_MINUTE * 0.70 -> "High"
    else -> "Limit risk"
}

private fun drivePressureLabel(unitsPerMinute: Double, errorsPerMinute: Double, reason: String): String = when {
    errorsPerMinute > 0.0 -> driveErrorReasonLabel(reason)
    else -> drivePressureLabel(unitsPerMinute, errorsPerMinute)
}

private fun driveErrorReasonLabel(reason: String): String {
    val value = reason.lowercase()
    val compact = value.filter { it.isLetterOrDigit() }
    return when {
        "storagequotaexceeded" in compact -> "Drive storage full"
        "ratelimit" in compact || "toomany" in compact || compact == "status429" -> "Drive rate limited"
        "invalidgrant" in compact || "unauthorized" in compact || compact == "status401" -> "Google access rejected"
        "notfound" in compact -> "Drive mailbox missing"
        "timeout" in compact || "deadline" in compact -> "Drive timeout"
        else -> "Drive errors"
    }
}

private fun performancePresetLabel(preset: String): String = when (preset) {
    PerformanceSettings.PRESET_LOWER_USAGE -> "Lower usage"
    PerformanceSettings.PRESET_RESPONSIVE -> "Responsive"
    PerformanceSettings.PRESET_BULK_TRANSFER -> "Bulk transfer"
    PerformanceSettings.PRESET_CUSTOM -> "Custom"
    else -> "Recommended"
}

private fun performancePresetDetail(performance: PerformanceSettings): String =
    "${performance.pollMs}ms · ${performance.uploadConcurrency}/${performance.downloadConcurrency}" +
        if (performance.burstPoll) " · burst" else ""

private const val SERVICE_STATE_GRACE_MS = 3_000L
private const val DRIVE_USER_UNITS_PER_MINUTE = 325_000.0

private fun File.readTail(maxBytes: Int, maxLines: Int): String {
    if (!exists() || length() == 0L) {
        return ""
    }
    val start = (length() - maxBytes).coerceAtLeast(0L)
    inputStream().use { input ->
        var remaining = start
        while (remaining > 0L) {
            val skipped = input.skip(remaining)
            if (skipped <= 0L) {
                break
            }
            remaining -= skipped
        }
        return input.bufferedReader()
            .readLines()
            .takeLast(maxLines)
            .joinToString("\n")
    }
}
